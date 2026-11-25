package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/relaunch"
)

// errSubagentsGate reports a stop refused because a seat may still have a subagent running.
var errSubagentsGate = errors.New("seat has live subagents")

// subagentTailBytes bounds the read of each subagent file: only its last record decides.
const subagentTailBytes = 64 * 1024

const subagentHeaderBytes = 8 * 1024

// changesFile is omp's per-session change log. It sits in the stem directory but is not a subagent.
const changesFile = "changes.jsonl"

// subagentFinished reports whether the last record marks a session finished. An unknown or partial
// last record is treated as running.
func subagentFinished(data []byte) bool {
	lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
	last := lines[len(lines)-1]
	if len(bytes.TrimSpace(last)) == 0 {
		return false
	}
	var record struct {
		Type       string `json:"type"`
		CustomType string `json:"customType"`
		Message    struct {
			Role       string `json:"role"`
			StopReason string `json:"stopReason"`
		} `json:"message"`
	}
	if json.Unmarshal(last, &record) != nil {
		return false
	}
	if record.Type == "custom" && record.CustomType == "session_exit" {
		return true
	}
	return record.Type == "message" && record.Message.Role == "assistant" &&
		(record.Message.StopReason == "stop" || record.Message.StopReason == "aborted")
}

// liveSubagents counts unfinished session files. A missing stem is zero; an unreadable file,
// unresolved session, or cancelled context is an error.
func liveSubagents(ctx context.Context, fileSystem relaunch.FileSystem, seatFile string, processStart time.Time) (int, error) {
	if seatFile == "" {
		return 0, errors.New("seat session file not resolved")
	}
	files, err := subagentFiles(fileSystem, strings.TrimSuffix(seatFile, ".jsonl"))
	if err != nil {
		return 0, err
	}
	running := 0
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		data, err := relaunch.ReadTail(fileSystem, file, subagentTailBytes)
		if err != nil {
			return 0, err
		}
		if subagentFinished(data) {
			continue
		}
		stale := false
		if !processStart.IsZero() {
			var prefix []byte
			if len(data) < subagentTailBytes {
				prefix = data[:min(len(data), subagentHeaderBytes)]
			} else if reader, ok := fileSystem.(relaunch.PrefixReader); ok {
				prefix, err = reader.ReadPrefix(file, subagentHeaderBytes)
				if err != nil {
					return 0, err
				}
			}
			for line := range bytes.SplitSeq(prefix, []byte("\n")) {
				var header struct {
					Type      string    `json:"type"`
					Timestamp time.Time `json:"timestamp"`
				}
				if json.Unmarshal(line, &header) != nil {
					break
				}
				if header.Type == "session" {
					stale = !header.Timestamp.IsZero() && header.Timestamp.Before(processStart.Truncate(time.Second))
					break
				}
			}
		}
		if !stale {
			running++
		}
	}
	return running, nil
}

// subagentFiles lists every subagent *.jsonl under stem, recursively. It descends only into real
// directories, skips omp's changes.jsonl, and ignores every other regular file. A missing stem is
// empty; a directory that cannot be listed is an error.
func subagentFiles(fileSystem relaunch.FileSystem, stem string) ([]string, error) {
	entries, err := fileSystem.ReadDir(stem)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var files []string
	for _, entry := range entries {
		path := filepath.Join(stem, entry.Name())
		if entry.IsDir() {
			nested, err := subagentFiles(fileSystem, path)
			if err != nil {
				return nil, err
			}
			files = append(files, nested...)
			continue
		}
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".jsonl") && entry.Name() != changesFile {
			files = append(files, path)
		}
	}
	return files, nil
}

// liveCheck counts a seat's running subagents. An error means unknown, and unknown counts as busy.
type liveCheck func(context.Context, string) (int, error)

// liveCheckFor builds the per-seat subagent check shared by seat stops and the memory step.
func liveCheckFor(procRoot string, fileSystem relaunch.FileSystem, sessionDir, waveSlice string) liveCheck {
	return func(ctx context.Context, name string) (int, error) {
		return liveSubagentsFor(ctx, procRoot, fileSystem, sessionDir, waveSlice, name)
	}
}

// liveSubagentsFor counts live subagents across the named seat's omp processes in the wave slice. A
// dead seat has none; an unnamed process, or one that cannot be read or resolved, is an error.
func liveSubagentsFor(ctx context.Context, procRoot string, fileSystem relaunch.FileSystem, sessionDir, waveSlice, name string) (int, error) {
	pids, err := gateSliceOmpPIDs(procRoot, fileSystem, waveSlice)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, pid := range pids {
		seat, gone, err := processSeatName(procRoot, fileSystem, pid)
		if err != nil {
			return 0, err
		}
		if gone {
			continue
		}
		if seat == "" {
			return 0, fmt.Errorf("omp process %s maps to no seat", pid)
		}
		if seat != name {
			continue
		}
		count, err := processLiveSubagents(ctx, procRoot, fileSystem, sessionDir, pid)
		if err != nil {
			return 0, fmt.Errorf("seat %s: %w", name, err)
		}
		total += count
	}
	return total, nil
}

// waveLiveSubagents checks every omp process in the slice. Any unreadable, unnamed, or busy process
// makes the whole-wave check fail, so an unknown state never permits a stop.
func waveLiveSubagents(ctx context.Context, procRoot string, fileSystem relaunch.FileSystem, sessionDir, waveSlice string) error {
	pids, err := gateSliceOmpPIDs(procRoot, fileSystem, waveSlice)
	if err != nil {
		return err
	}
	for _, pid := range pids {
		if err := ctx.Err(); err != nil {
			return err
		}
		name, gone, err := processSeatName(procRoot, fileSystem, pid)
		if err != nil {
			return err
		}
		if gone {
			continue
		}
		if name == "" {
			return fmt.Errorf("omp process %s maps to no seat", pid)
		}
		count, err := processLiveSubagents(ctx, procRoot, fileSystem, sessionDir, pid)
		if err != nil {
			return fmt.Errorf("seat %s: %w", name, err)
		}
		if count > 0 {
			return fmt.Errorf("%w: %s has %d running", errSubagentsGate, name, count)
		}
	}
	return nil
}

// gateSliceOmpPIDs lists omp processes in the configured wave slice. Exited processes are skipped;
// any other cgroup or command-line read error refuses the stop.
func gateSliceOmpPIDs(procRoot string, fileSystem relaunch.FileSystem, waveSlice string) ([]string, error) {
	entries, err := fileSystem.ReadDir(procRoot)
	if err != nil {
		return nil, fmt.Errorf("cannot enumerate proc: %w", err)
	}
	pids := make([]string, 0)
	for _, entry := range entries {
		pid := entry.Name()
		if !decimalDigits(pid) {
			continue
		}
		cgroup, gone, err := readProcFile(fileSystem, filepath.Join(procRoot, pid, "cgroup"))
		if err != nil {
			return nil, err
		}
		if gone || !cgroupInWaveSlice(string(cgroup), waveSlice) {
			continue
		}
		cmdline, gone, err := readProcFile(fileSystem, filepath.Join(procRoot, pid, "cmdline"))
		if err != nil {
			return nil, err
		}
		if !gone && isOmpProcess(string(cmdline)) {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// readProcFile reads one /proc file. gone is true when the process has exited.
func readProcFile(fileSystem relaunch.FileSystem, path string) (data []byte, gone bool, err error) {
	data, err = fileSystem.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("cannot read %s: %w", path, err)
	}
	return data, false, nil
}

// processSeatName reads the seat name of one omp process. An exited process has no subagents; other
// read errors are returned.
func processSeatName(procRoot string, fileSystem relaunch.FileSystem, pid string) (name string, gone bool, err error) {
	environ, err := fileSystem.ReadFile(filepath.Join(procRoot, pid, "environ"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", true, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("cannot read environment for omp process %s: %w", pid, err)
	}
	return processAgentName(string(environ)), false, nil
}

// processLiveSubagents resolves one omp process to its top-level session file and counts files under
// that file's stem. A process that exited since it was listed has zero.
func processLiveSubagents(ctx context.Context, procRoot string, fileSystem relaunch.FileSystem, sessionDir, pid string) (int, error) {
	base := filepath.Join(procRoot, pid)
	cmdline, err := fileSystem.ReadFile(filepath.Join(base, "cmdline"))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("cannot read command line for omp process %s: %w", pid, err)
	}
	environ, err := fileSystem.ReadFile(filepath.Join(base, "environ"))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("cannot read environment for omp process %s: %w", pid, err)
	}
	resolved := resolveProcessSession(base, string(cmdline), environ, true, sessionDir, fileSystem)
	if resolved == nil || resolved.ID == "" {
		return 0, fmt.Errorf("session not found for omp process %s", pid)
	}
	if resolved.Stale {
		return 0, fmt.Errorf("session for omp process %s not resolved (stale guess %s)", pid, resolved.ID)
	}
	seatFile, err := topLevelSessionFile(sessionDir, resolved.ID, fileSystem)
	if err != nil {
		return 0, err
	}
	return liveSubagents(ctx, fileSystem, seatFile, processStartTime(base, fileSystem))
}

// topLevelSessionFile finds the seat's top-level session file under the session root. omp names it
// `<timestamp>_<session-id>.jsonl`, so a name match needs only directory listings. With no name
// match it falls back to each top-level file's header, read from a bounded prefix. A missing or
// ambiguous match is an error, never a guess.
func topLevelSessionFile(sessionDir, sessionID string, fileSystem relaunch.FileSystem) (string, error) {
	dirs, err := fileSystem.ReadDir(sessionDir)
	if err != nil {
		return "", err
	}
	suffix := "_" + sessionID + ".jsonl"
	var named, all []string
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		entries, err := fileSystem.ReadDir(filepath.Join(sessionDir, dir.Name()))
		if err != nil {
			return "", err
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(sessionDir, dir.Name(), entry.Name())
			all = append(all, path)
			if strings.HasSuffix(entry.Name(), suffix) {
				named = append(named, path)
			}
		}
	}
	found := named
	if len(found) == 0 {
		for _, file := range all {
			if header, ok := relaunch.ReadSessionHeader(fileSystem, file); ok && header.ID == sessionID {
				found = append(found, file)
			}
		}
	}
	if len(found) != 1 {
		return "", fmt.Errorf("session %s resolved to %d top-level files", sessionID, len(found))
	}
	return found[0], nil
}
