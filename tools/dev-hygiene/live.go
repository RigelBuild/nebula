package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// lsofPath is the absolute path on macOS. A launchd PATH may not carry /usr/sbin.
const lsofPath = "/usr/sbin/lsof"

// liveCwds returns the working directory of every process this user can read, once per run.
// Linux reads /proc; elsewhere it asks lsof. Any failure is an error, never an empty set.
func liveCwds(ctx context.Context) ([]string, error) {
	if _, err := os.Stat("/proc/self/cwd"); err == nil {
		return liveCwdsProc()
	}
	return liveCwdsLsof(ctx)
}

func liveCwdsProc() ([]string, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("read /proc: %w", err)
	}
	var cwds []string
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		target, err := os.Readlink(filepath.Join("/proc", entry.Name(), "cwd"))
		switch {
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, fs.ErrPermission):
			// The process exited, or another user owns it; only same-user processes sit in our workspaces.
			continue
		case err != nil:
			return nil, fmt.Errorf("read cwd of pid %s: %w", entry.Name(), err)
		}
		cwds = append(cwds, target)
	}
	return cwds, nil
}

// liveCwdsLsof lists cwd records as lsof -F n lines. Any non-zero exit is an error: lsof lists its own cwd,
// so exit 1 means it failed; an empty result would treat workspaces as not live.
func liveCwdsLsof(ctx context.Context) ([]string, error) {
	result, err := commandRun(ctx, "", nil, lsofPath, "-a", "-d", "cwd", "-Fn", "-w")
	if err != nil {
		return nil, fmt.Errorf("list process cwds: %w", err)
	}
	return parseLsofCwds(result.stdout)
}

// parseLsofCwds reads lsof -F n output. No cwd records is an error, never an empty set.
func parseLsofCwds(text string) ([]string, error) {
	var cwds []string
	for line := range strings.SplitSeq(text, "\n") {
		if path, ok := strings.CutPrefix(line, "n"); ok && path != "" {
			cwds = append(cwds, path)
		}
	}
	if len(cwds) == 0 {
		return nil, errors.New("lsof reported no cwd records; refusing to assume none are live")
	}
	return cwds, nil
}

// hasLiveCwd reports whether any process cwd in live lies at root or beneath it.
func hasLiveCwd(root string, live []string) bool {
	for _, cwd := range live {
		if within(cwd, root) {
			return true
		}
	}
	return false
}

// within reports whether path equals root or lies beneath it.
func within(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}
