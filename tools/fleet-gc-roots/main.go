// SPDX-License-Identifier: MIT OR Apache-2.0

// Package main lists Nix GC roots that live in idle agent workspaces. It deletes nothing.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec" //nolint:depguard // Fixed nix-store argv, no shell; the GC root query needs the store.
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defaultProc = "/proc"
	idleAfter   = 7 * 24 * time.Hour

	// The caller names the host paths that the run scans and writes. Neither has a default.
	envWorkspaces = "FLEET_GC_ROOTS_WORKSPACES"
	envOutput     = "FLEET_GC_ROOTS_OUTPUT"
)

// candidates is one dry-run pass over the root listing.
type candidates struct {
	Roots      []string // workspace GC root links inside idle workspaces
	Workspaces int      // idle workspaces that hold at least one candidate root
	Busy       int      // idle workspaces skipped because a process has them as cwd
	Unparsed   int      // root lines that did not parse
	Unreadable int      // /proc entries whose cwd could not be read
	Vanished   int      // root links whose workspace was deleted or replaced during the run
}

func main() {
	os.Exit(runFromEnvironment(os.Args[1:], os.Stdout, os.Stderr, os.Getenv))
}

// runFromEnvironment reads the workspace tree and report path from the environment.
func runFromEnvironment(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	workspaces := getenv(envWorkspaces)
	output := getenv(envOutput)
	if workspaces == "" || output == "" {
		slog.New(slog.NewTextHandler(stderr, nil)).Error("fleet-gc-roots needs both paths", "workspaces_env", envWorkspaces, "output_env", envOutput)
		return 2
	}
	return run(args, stdout, stderr, runEnv{
		workspaces: workspaces,
		procRoot:   defaultProc,
		output:     output,
		now:        time.Now(),
		gcRoots:    nixRoots,
	})
}

// runEnv is the input and output of one run. main fills it from the host;
// tests fill it with temporary paths and fixed input.
type runEnv struct {
	workspaces string
	procRoot   string
	output     string
	now        time.Time
	gcRoots    func() ([]string, error)
}

func run(args []string, stdout, stderr io.Writer, env runEnv) int {
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	if len(args) != 0 {
		logger.Error("fleet-gc-roots takes no arguments")
		return 2
	}
	roots, err := env.gcRoots()
	if err != nil {
		logger.Error("list nix GC roots", "error", err)
		return 1
	}
	found, err := findCandidates(env.workspaces, env.procRoot, roots, env.now, idleAfter)
	if err != nil {
		logger.Error("find candidates", "error", err)
		return 1
	}
	report := renderReport(found, env.now, idleAfter, env.output)
	if _, err := io.WriteString(stdout, report); err != nil {
		logger.Error("write report to stdout", "error", err)
		return 1
	}
	if err := writeFile(env.output, report); err != nil {
		logger.Error("write candidates file", "path", env.output, "error", err)
		return 1
	}
	return 0
}

// nixRoots returns the raw `nix-store --gc --print-roots` lines.
func nixRoots() ([]string, error) {
	out, err := exec.Command("nix-store", "--gc", "--print-roots").Output() // #nosec G204 -- fixed argv, no shell.
	if err != nil {
		return nil, fmt.Errorf("nix-store --gc --print-roots: %w", err)
	}
	var lines []string
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, scanner.Err()
}

// parseRootLine returns the link path of one root line: `"<link>" -> <store path>`.
func parseRootLine(line string) (string, bool) {
	sep := strings.LastIndex(line, `" -> `)
	if sep < 0 || !strings.HasPrefix(line, `"`) {
		return "", false
	}
	link, err := strconv.Unquote(line[:sep+1])
	if err != nil {
		return "", false
	}
	return link, true
}

// relParts splits link into its path elements below workspaces.
// It reports false when link is not strictly inside workspaces.
func relParts(link, workspaces string) ([]string, bool) {
	rel, err := filepath.Rel(workspaces, link)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, false
	}
	return strings.Split(filepath.ToSlash(rel), "/"), true
}

// isWorkspaceRoot reports whether link is a root inside a workspace: a devenv or direnv
// path part, or a result link at any depth below <seat>/<ws>.
func isWorkspaceRoot(link, workspaces string) bool {
	parts, ok := relParts(link, workspaces)
	if !ok || len(parts) < 3 {
		return false
	}
	// parts[0] is the seat and parts[1] the workspace; the root sits below them.
	inside := parts[2:]
	for _, part := range inside {
		if part == ".devenv" || part == ".direnv" {
			return true
		}
	}
	base := inside[len(inside)-1]
	return base == "result" || strings.HasPrefix(base, "result-")
}

// workspaceDir returns <workspaces>/<seat>/<ws> for a link, or "" if it is outside.
func workspaceDir(link, workspaces string) string {
	parts, ok := relParts(link, workspaces)
	if !ok || len(parts) < 2 {
		return ""
	}
	return filepath.Join(workspaces, parts[0], parts[1])
}

// findCandidates lists workspace roots whose workspace has been idle longer than idle.
// Idle is judged by newestActivity, which walks from the workspace down to the root's parent.
func findCandidates(workspaces, procRoot string, rootLines []string, now time.Time, idle time.Duration) (candidates, error) {
	var result candidates
	busy, unreadable, err := busyWorkspaces(procRoot, workspaces)
	if err != nil {
		return result, err
	}
	result.Unreadable = unreadable
	seenBusy := map[string]bool{}
	hits := map[string]bool{}
	for _, line := range rootLines {
		link, ok := parseRootLine(line)
		if !ok {
			result.Unparsed++
			continue
		}
		if !isWorkspaceRoot(link, workspaces) {
			continue
		}
		dir := workspaceDir(link, workspaces)
		if dir == "" {
			continue
		}
		if busy[dir] {
			seenBusy[dir] = true
			continue
		}
		stale, err := isIdle(workspaces, link, now, idle)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			// The workspace was deleted or replaced by a file after nix listed the root.
			result.Vanished++
			continue
		}
		if err != nil {
			return result, err
		}
		if !stale {
			continue
		}
		hits[dir] = true
		result.Roots = append(result.Roots, link)
	}
	result.Busy = len(seenBusy)
	result.Workspaces = len(hits)
	sort.Strings(result.Roots)
	return result, nil
}

// isIdle reports whether the workspace holding link has had no activity for longer than idle.
func isIdle(workspaces, link string, now time.Time, idle time.Duration) (bool, error) {
	newest, err := newestActivity(workspaces, link)
	if err != nil {
		return false, err
	}
	return now.Sub(newest) > idle, nil
}

// newestActivity is the newest mtime of the workspace holding link. It counts the workspace
// directory and the .jj/working_copy of every directory from <seat>/<ws> down to the root's
// parent, so a nested jj workspace such as team/agent-3/repo counts its own working
// copy and not only its container's. The shared .jj/repo op log is skipped: every workspace
// writes to it.
func newestActivity(workspaces, link string) (time.Time, error) {
	parts, ok := relParts(link, workspaces)
	if !ok || len(parts) < 3 {
		return time.Time{}, fmt.Errorf("%s is not inside a workspace root", link)
	}
	// The chain runs from <seat>/<ws> down to the root's parent: parts[2:len(parts)-1].
	dirs := []string{filepath.Join(workspaces, parts[0], parts[1])}
	for _, part := range parts[2 : len(parts)-1] {
		dirs = append(dirs, filepath.Join(dirs[len(dirs)-1], part))
	}
	info, err := os.Stat(dirs[0])
	if err != nil {
		return time.Time{}, err
	}
	newest := info.ModTime()
	for _, d := range dirs {
		wc, statErr := os.Stat(filepath.Join(d, ".jj", "working_copy"))
		if errors.Is(statErr, fs.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return time.Time{}, statErr
		}
		if wc.ModTime().After(newest) {
			newest = wc.ModTime()
		}
	}
	return newest, nil
}

// busyWorkspaces returns the workspace dirs that some process has as cwd.
func busyWorkspaces(procRoot, workspaces string) (map[string]bool, int, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, 0, fmt.Errorf("read %s: %w", procRoot, err)
	}
	busy := map[string]bool{}
	unreadable := 0
	for _, entry := range entries {
		if !isPID(entry.Name()) {
			continue
		}
		cwd, err := os.Readlink(filepath.Join(procRoot, entry.Name(), "cwd"))
		if err != nil {
			unreadable++
			continue
		}
		if dir := workspaceDir(cwd, workspaces); dir != "" {
			busy[dir] = true
		}
	}
	return busy, unreadable, nil
}

func isPID(name string) bool {
	_, err := strconv.Atoi(name)
	return err == nil
}

// renderReport builds the summary line plus one candidate root per line.
func renderReport(found candidates, now time.Time, idle time.Duration, outputPath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "fleet-gc-roots: %d idle workspace GC root(s) in %d workspace(s); dry run, nothing deleted\n", len(found.Roots), found.Workspaces)
	fmt.Fprintf(&b, "generated %s, idle after %s, output %s\n", now.Format(time.RFC3339), idle, outputPath)
	fmt.Fprintf(&b, "skipped busy workspaces: %d; unparsed root lines: %d; unreadable proc cwd: %d; vanished roots: %d\n", found.Busy, found.Unparsed, found.Unreadable, found.Vanished)
	for _, root := range found.Roots {
		b.WriteString(root)
		b.WriteByte('\n')
	}
	return b.String()
}

// writeFile replaces path atomically with content.
func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o640); err != nil { // #nosec G306 -- the report lists paths only.
		return err
	}
	return os.Rename(tmp, path)
}
