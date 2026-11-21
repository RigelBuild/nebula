package watchdog

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/RigelBuild/nebula/tools/zellij-rss-watchdog/internal/command"
	"github.com/RigelBuild/nebula/tools/zellij-rss-watchdog/internal/proc"
	"github.com/RigelBuild/nebula/tools/zellij-rss-watchdog/internal/seam"
)

const (
	// levelError routes a log line to stderr.
	levelError = "error"
)

// systemIO is the production seam.IO backed by /proc and the real filesystem.
type systemIO struct {
	cfg                 seam.Config
	stdout              io.Writer
	stderr              io.Writer
	previousUnavailable bool
}

func newSystemIO(cfg seam.Config, stdout, stderr io.Writer) *systemIO {
	return &systemIO{
		cfg:    cfg,
		stdout: stdout,
		stderr: stderr,
	}
}

func (s *systemIO) Config() seam.Config { return s.cfg }

func (s *systemIO) Now() int64 { return time.Now().UnixMilli() }

func (s *systemIO) Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *systemIO) ReadDir(path string) ([]string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, nil
}

func (s *systemIO) ReadText(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is a proc or watchdog state path the watchdog builds itself, never external input.
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (s *systemIO) CopyMemory(pid string, start, end uint64, outputPath, identity string) error {
	return proc.CopyProcMemory(s.cfg.ProcRoot, pid, start, end, outputPath, identity)
}

func (s *systemIO) Identity() string {
	return proc.SessionServerIdentity(s.cfg.ProcRoot, s.cfg.RuntimeDir, s.cfg.Session)
}

func (s *systemIO) ReadUserLogs(ctx context.Context, maxBytes int64) (seam.LogRead, error) {
	entries, err := listUserLogs(ctx, s.cfg.LogDirectory)
	if err != nil {
		return seam.LogRead{}, err
	}
	return readUserLogs(maxBytes, entries, func(name string, count int64) ([]byte, error) {
		return proc.ReadLogTail(s.cfg.LogDirectory, name, count)
	}), nil
}

// Captures are published group-readable so the server user can read them.
func (s *systemIO) WriteText(path, text string) error {
	return os.WriteFile(path, []byte(text), 0o640) //nolint:gosec // G306: 0640 is the published capture mode; group users reads the captures.
}

func (s *systemIO) WriteBytes(path string, data []byte) error {
	return os.WriteFile(path, data, 0o640) //nolint:gosec // G306: 0640 is the published capture mode; group users reads the captures.
}

func (s *systemIO) Stat(path string) (seam.DirEntry, error) {
	info, err := os.Stat(path)
	if err != nil {
		return seam.DirEntry{}, err
	}
	return seam.DirEntry{Name: filepath.Base(path), IsDirectory: info.IsDir(), IsFile: info.Mode().IsRegular()}, nil
}

func (s *systemIO) Mkdir(path string) error {
	return os.MkdirAll(path, 0o750)
}

func (s *systemIO) Remove(path string) error {
	return os.RemoveAll(path)
}

func (s *systemIO) Run(ctx context.Context, name string, args []string, timeout time.Duration) (command.Result, error) {
	return command.Text(ctx, name, args, timeout)
}

func (s *systemIO) RecordUnavailableTransition(identity string) bool {
	unavailable := identity == proc.IdentityUnavailable
	changed := unavailable && !s.previousUnavailable
	s.previousUnavailable = unavailable
	return changed
}

// Log writes one line; errors go to stderr and everything else to stdout.
func (s *systemIO) Log(level, message string) {
	line := fmt.Sprintf("zellij-rss-watchdog: level=%s %s\n", level, message)
	out := s.stdout
	if level == levelError {
		out = s.stderr
	}
	// A failed log write cannot reach the same stream; report it to the process logger and keep sampling.
	if _, err := io.WriteString(out, line); err != nil {
		slog.Error("writing watchdog log line", "error", err)
	}
}

// readUserLogs returns the newest log tails under maxBytes, newest first. A tail that cannot be
// read is recorded in errors and skipped, so one bad file does not hide the others.
func readUserLogs(maxBytes int64, entries []seam.UserLogEntry, readTail func(name string, count int64) ([]byte, error)) seam.LogRead {
	sorted := slices.Clone(entries)
	slices.SortStableFunc(sorted, func(a, b seam.UserLogEntry) int {
		switch {
		case a.Modified > b.Modified:
			return -1
		case a.Modified < b.Modified:
			return 1
		default:
			return 0
		}
	})
	var result seam.LogRead
	remaining := maxBytes
	for _, entry := range sorted {
		if remaining <= 0 {
			break
		}
		count := min(entry.Size, remaining)
		contents, err := readTail(entry.Name, count)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %s", entry.Name, err))
			continue
		}
		if int64(len(contents)) > count {
			contents = contents[int64(len(contents))-count:]
		}
		result.Files = append(result.Files, seam.LogFile{Name: entry.Name, Contents: contents})
		remaining -= int64(len(contents))
	}
	return result
}

// listUserLogs lists regular files in the zellij log directory through find.
func listUserLogs(ctx context.Context, directory string) ([]seam.UserLogEntry, error) {
	listed, err := command.Text(ctx, "find", []string{directory, "-maxdepth", "1", "-type", "f", "-printf", `%T@\0%s\0%f\0`}, command.CommandTimeout)
	if err != nil {
		return nil, err
	}
	fields := strings.Split(listed.Stdout, "\x00")
	var entries []seam.UserLogEntry
	for i := 0; i+2 < len(fields); i += 3 {
		modified, errModified := strconv.ParseFloat(fields[i], 64)
		size, errSize := strconv.ParseInt(fields[i+1], 10, 64)
		name := fields[i+2]
		if name != "" && errModified == nil && errSize == nil && size <= proc.MaxSafeInteger {
			entries = append(entries, seam.UserLogEntry{Name: name, Modified: modified, Size: size})
		}
	}
	return entries, nil
}
