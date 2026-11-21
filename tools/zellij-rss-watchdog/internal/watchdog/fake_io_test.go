package watchdog

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/zellij-rss-watchdog/internal/command"
	"github.com/RigelBuild/nebula/tools/zellij-rss-watchdog/internal/seam"
)

var _ seam.IO = (*fakeIO)(nil)

const fakeNowMS int64 = 1_798_761_600_000

type fakeLog struct {
	level   string
	message string
}

// fakeIO is an in-memory seam.IO. A hook replaces its method when set;
// an unset hook falls back to the in-memory default.
type fakeIO struct {
	cfg             seam.Config
	clockFn         func() int64
	identityFn      func() string
	readTextFn      func(path string) (string, error)
	writeTextFn     func(path, text string) error
	writeBytesFn    func(path string, data []byte) error
	removeFn        func(path string) error
	runFn           func(ctx context.Context, name string, args []string, timeout time.Duration) (command.Result, error)
	copyFn          func(pid string, start, end uint64, outputPath, identity string) error
	userLogsFn      func(ctx context.Context, maxBytes int64) (seam.LogRead, error)
	files           map[string]string
	dirs            map[string][]string
	logs            []fakeLog
	unavailableLast bool
}

func newFakeIO() *fakeIO {
	return &fakeIO{
		cfg: seam.Config{
			ProcRoot:       "/proc",
			RuntimeDir:     "/run/user/1000",
			CaptureRoot:    "/captures",
			RelaunchBinary: "/example/bin/relaunch",
			User:           testUser,
			Session:        testSession,
			LogDirectory:   "/tmp/example-log",
			JournalUnit:    testJournalUnit,
			ClientTmpDir:   "/example/client-tmp",
		},
		clockFn:    func() int64 { return fakeNowMS },
		identityFn: func() string { return "123:456" },
		files: map[string]string{
			"/proc/123/status": "Name:\tzellij\nVmRSS:\t2000000 kB\n",
			"/proc/123/smaps":  "",
		},
		dirs: map[string][]string{
			"/captures": {},
			"/logs":     {},
		},
	}
}

func (f *fakeIO) Config() seam.Config { return f.cfg }

func (f *fakeIO) Now() int64 { return f.clockFn() }

func (f *fakeIO) Sleep(context.Context, time.Duration) error { return nil }

func (f *fakeIO) Identity() string { return f.identityFn() }

func (f *fakeIO) ReadDir(path string) ([]string, error) {
	return slices.Clone(f.dirs[path]), nil
}

func (f *fakeIO) ReadText(path string) (string, error) {
	if f.readTextFn != nil {
		return f.readTextFn(path)
	}
	value, ok := f.files[path]
	if !ok {
		return "", fmt.Errorf("missing %s: %w", path, fs.ErrNotExist)
	}
	return value, nil
}

func (f *fakeIO) CopyMemory(pid string, start, end uint64, outputPath, identity string) error {
	if f.copyFn != nil {
		return f.copyFn(pid, start, end, outputPath, identity)
	}
	f.files[outputPath] = "memory"
	return nil
}

func (f *fakeIO) ReadUserLogs(ctx context.Context, maxBytes int64) (seam.LogRead, error) {
	if f.userLogsFn != nil {
		return f.userLogsFn(ctx, maxBytes)
	}
	return seam.LogRead{}, nil
}

func (f *fakeIO) WriteText(path, text string) error {
	if f.writeTextFn != nil {
		return f.writeTextFn(path, text)
	}
	f.files[path] = text
	f.link(path)
	return nil
}

func (f *fakeIO) WriteBytes(path string, data []byte) error {
	if f.writeBytesFn != nil {
		return f.writeBytesFn(path, data)
	}
	return nil
}

func (f *fakeIO) Stat(path string) (seam.DirEntry, error) {
	_, isDir := f.dirs[path]
	_, isFile := f.files[path]
	return seam.DirEntry{Name: filepath.Base(path), IsDirectory: isDir, IsFile: isFile}, nil
}

func (f *fakeIO) Mkdir(path string) error {
	f.link(path)
	if _, ok := f.dirs[path]; !ok {
		f.dirs[path] = []string{}
	}
	return nil
}

func (f *fakeIO) Remove(path string) error {
	if f.removeFn != nil {
		return f.removeFn(path)
	}
	delete(f.files, path)
	delete(f.dirs, path)
	parent, child := filepath.Dir(path), filepath.Base(path)
	if listing, ok := f.dirs[parent]; ok {
		f.dirs[parent] = slices.DeleteFunc(listing, func(name string) bool { return name == child })
	}
	return nil
}

func (f *fakeIO) Run(ctx context.Context, name string, args []string, timeout time.Duration) (command.Result, error) {
	if f.runFn != nil {
		return f.runFn(ctx, name, args, timeout)
	}
	switch {
	case name == "ss" || name == "bash":
		return command.Result{}, nil
	case name == "runuser" && slices.Contains(args, "journalctl"):
		return command.Result{Stdout: "journal"}, nil
	case name == "runuser" && slices.Contains(args, "zellij"):
		return command.Result{Stdout: "clients"}, nil
	default:
		return command.Result{}, fmt.Errorf("unexpected command: %s %s", name, strings.Join(args, " "))
	}
}

func (f *fakeIO) RecordUnavailableTransition(identity string) bool {
	unavailable := identity == "unavailable"
	changed := unavailable && !f.unavailableLast
	f.unavailableLast = unavailable
	return changed
}

func (f *fakeIO) Log(level, message string) {
	f.logs = append(f.logs, fakeLog{level: level, message: message})
}

// logged reports whether any log line at level contains substring.
func (f *fakeIO) logged(level, substring string) bool {
	for _, entry := range f.logs {
		if entry.level == level && strings.Contains(entry.message, substring) {
			return true
		}
	}
	return false
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func readStateText(t *testing.T, text string) watchState {
	t.Helper()
	var state watchState
	if err := json.Unmarshal([]byte(text), &state); err != nil {
		t.Fatalf("state is not valid JSON: %v", err)
	}
	return state
}

// link adds path to its parent listing when the parent is a known directory.
func (f *fakeIO) link(path string) {
	parent, child := filepath.Dir(path), filepath.Base(path)
	if _, ok := f.dirs[parent]; !ok {
		return
	}
	if !slices.Contains(f.dirs[parent], child) {
		f.dirs[parent] = append(f.dirs[parent], child)
	}
}

// Example values only: the operator sets the real user and session with required flags.
const (
	testUser        = "example-user"
	testSession     = "example-session"
	testJournalUnit = "example-unit"
)
