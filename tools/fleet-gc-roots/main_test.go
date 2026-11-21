// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testIdle = 7 * 24 * time.Hour

var testNow = time.Date(2026, 11, 17, 4, 0, 0, 0, time.UTC)

// makeWorkspace creates <workspaces>/<seat>/<name> with its mtime age before testNow.
func makeWorkspace(t *testing.T, workspaces, seat, name string, age time.Duration) string {
	t.Helper()
	dir := filepath.Join(workspaces, seat, name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	mtime := testNow.Add(-age)
	if err := os.Chtimes(dir, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return dir
}

// rootLine formats one `nix-store --gc --print-roots` row for link.
func rootLine(link string) string {
	return strconv.Quote(link) + " -> /nix/store/aaaa-devenv-shell"
}

func TestFindCandidatesListsOnlyIdleWorkspaceRoots(t *testing.T) {
	t.Parallel()
	tree := t.TempDir()
	workspaces := filepath.Join(tree, "workspaces")
	procRoot := filepath.Join(tree, "proc")

	idle := makeWorkspace(t, workspaces, "seat-a", "ws-idle", 8*24*time.Hour)
	fresh := makeWorkspace(t, workspaces, "seat-a", "ws-fresh", 24*time.Hour)
	boundary := makeWorkspace(t, workspaces, "seat-b", "ws-boundary", testIdle)
	busy := makeWorkspace(t, workspaces, "seat-b", "ws-busy", 9*24*time.Hour)

	// A process with its cwd inside an idle workspace marks that workspace active.
	if err := os.MkdirAll(filepath.Join(procRoot, "4242"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(busy, "src"), filepath.Join(procRoot, "4242", "cwd")); err != nil {
		t.Fatal(err)
	}
	// A process outside every workspace must not skip anything.
	if err := os.MkdirAll(filepath.Join(procRoot, "77"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/srv/example/elsewhere", filepath.Join(procRoot, "77", "cwd")); err != nil {
		t.Fatal(err)
	}

	roots := []string{
		rootLine(filepath.Join(idle, ".devenv", "gc", "shell")),
		rootLine(filepath.Join(idle, ".direnv", "flake-profile")),
		rootLine(filepath.Join(idle, "result")),
		rootLine(filepath.Join(idle, "sub", ".devenv", "gc", "shell")),
		rootLine(filepath.Join(idle, ".devenv", "bash-bash")),
		rootLine(filepath.Join(idle, ".devenv", "profiles", "p", "gc", "shell")),
		rootLine(filepath.Join(idle, "infra", "nix", "result-1")),
		rootLine(filepath.Join(idle, "src", "main-link")),
		rootLine(filepath.Join(fresh, ".devenv", "gc", "shell")),
		rootLine(filepath.Join(boundary, "result")),
		rootLine(filepath.Join(busy, ".devenv", "gc", "shell")),
		rootLine("/nix/var/nix/profiles/system"),
		rootLine("{temp:1159349}"),
		"not a root line",
	}

	got, err := findCandidates(workspaces, procRoot, roots, testNow, testIdle)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(idle, ".devenv", "gc", "shell"),
		filepath.Join(idle, ".direnv", "flake-profile"),
		filepath.Join(idle, "result"),
		filepath.Join(idle, "sub", ".devenv", "gc", "shell"),
		filepath.Join(idle, ".devenv", "bash-bash"),
		filepath.Join(idle, ".devenv", "profiles", "p", "gc", "shell"),
		filepath.Join(idle, "infra", "nix", "result-1"),
	}
	sort.Strings(want)
	if !slices.Equal(got.Roots, want) {
		t.Fatalf("candidate roots = %q, want %q", got.Roots, want)
	}
	if got.Workspaces != 1 {
		t.Fatalf("candidate workspaces = %d, want 1", got.Workspaces)
	}
	if got.Busy != 1 {
		t.Fatalf("busy workspaces skipped by cwd = %d, want 1", got.Busy)
	}
	if got.Unparsed != 1 {
		t.Fatalf("unparsed root lines = %d, want 1", got.Unparsed)
	}
	if got.Unreadable != 0 {
		t.Fatalf("unreadable proc cwd entries = %d, want 0", got.Unreadable)
	}

	report := renderReport(got, testNow, testIdle, "/srv/example/state/candidates.txt")
	firstLine := strings.SplitN(report, "\n", 2)[0]
	wantFirst := "fleet-gc-roots: 7 idle workspace GC root(s) in 1 workspace(s); dry run, nothing deleted"
	if firstLine != wantFirst {
		t.Fatalf("summary line = %q, want %q", firstLine, wantFirst)
	}
	if !strings.Contains(report, filepath.Join(idle, "result")) {
		t.Fatalf("report does not list candidate root:\n%s", report)
	}
}

func TestFindCandidatesCountsNestedWorkspaceActivity(t *testing.T) {
	t.Parallel()
	tree := t.TempDir()
	workspaces := filepath.Join(tree, "workspaces")
	procRoot := filepath.Join(tree, "proc")
	if err := os.MkdirAll(procRoot, 0o750); err != nil {
		t.Fatal(err)
	}

	idle := makeWorkspace(t, workspaces, "seat-d", "ws-outer", 8*24*time.Hour)
	// Creating inner moves idle's mtime, so reset it after the nested workspace exists.
	inner := filepath.Join(idle, "inner")
	wc := filepath.Join(inner, ".jj", "working_copy")
	if err := os.MkdirAll(wc, 0o750); err != nil {
		t.Fatal(err)
	}
	outerTime := testNow.Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(idle, outerTime, outerTime); err != nil {
		t.Fatal(err)
	}
	wcTime := testNow.Add(-time.Hour)
	if err := os.Chtimes(wc, wcTime, wcTime); err != nil {
		t.Fatal(err)
	}

	roots := []string{
		rootLine(filepath.Join(inner, ".devenv", "gc", "shell")),
		rootLine(filepath.Join(idle, ".devenv", "gc", "shell")),
	}
	got, err := findCandidates(workspaces, procRoot, roots, testNow, testIdle)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(idle, ".devenv", "gc", "shell")}
	if !slices.Equal(got.Roots, want) {
		t.Fatalf("candidate roots = %q, want %q", got.Roots, want)
	}
	if got.Workspaces != 1 {
		t.Fatalf("candidate workspaces = %d, want 1", got.Workspaces)
	}
}

func TestFindCandidatesSkipsWorkspaceDeletedMidRun(t *testing.T) {
	t.Parallel()
	tree := t.TempDir()
	workspaces := filepath.Join(tree, "workspaces")
	procRoot := filepath.Join(tree, "proc")
	if err := os.MkdirAll(procRoot, 0o750); err != nil {
		t.Fatal(err)
	}

	idle := makeWorkspace(t, workspaces, "seat-a", "ws-idle", 8*24*time.Hour)
	// The root was listed by nix, then its workspace was deleted before the stat.
	gone := filepath.Join(workspaces, "seat-a", "ws-gone")
	// Another workspace was replaced by a file before the stat.
	replaced := filepath.Join(workspaces, "seat-a", "ws-file")
	if err := os.WriteFile(replaced, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	roots := []string{
		rootLine(filepath.Join(gone, ".devenv", "gc", "shell")),
		rootLine(filepath.Join(replaced, ".devenv", "gc", "shell")),
		rootLine(filepath.Join(idle, ".devenv", "gc", "shell")),
	}
	got, err := findCandidates(workspaces, procRoot, roots, testNow, testIdle)
	if err != nil {
		t.Fatalf("findCandidates error = %v, want nil", err)
	}
	want := []string{filepath.Join(idle, ".devenv", "gc", "shell")}
	if !slices.Equal(got.Roots, want) {
		t.Fatalf("candidate roots = %q, want %q", got.Roots, want)
	}
	if got.Vanished != 2 {
		t.Fatalf("vanished roots = %d, want 2", got.Vanished)
	}
}

func TestIsIdleUsesNewerOfDirAndWorkingCopy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		dirAge time.Duration
		wcAge  time.Duration
		idle   bool
	}{
		{name: "working copy fresh, dir old", dirAge: 8 * 24 * time.Hour, wcAge: time.Hour, idle: false},
		{name: "dir fresh, working copy old", dirAge: time.Hour, wcAge: 8 * 24 * time.Hour, idle: false},
		{name: "both old", dirAge: 8 * 24 * time.Hour, wcAge: 9 * 24 * time.Hour, idle: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tree := t.TempDir()
			dir := makeWorkspace(t, tree, "seat-c", "ws-wc", tc.dirAge)
			wc := filepath.Join(dir, ".jj", "working_copy")
			if err := os.MkdirAll(wc, 0o750); err != nil {
				t.Fatal(err)
			}
			// Creating .jj moved the directory mtime, so set both ages after it.
			dirTime := testNow.Add(-tc.dirAge)
			if err := os.Chtimes(dir, dirTime, dirTime); err != nil {
				t.Fatal(err)
			}
			wcTime := testNow.Add(-tc.wcAge)
			if err := os.Chtimes(wc, wcTime, wcTime); err != nil {
				t.Fatal(err)
			}
			got, err := isIdle(tree, filepath.Join(dir, ".devenv", "gc", "shell"), testNow, testIdle)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.idle {
				t.Fatalf("isIdle = %t, want %t", got, tc.idle)
			}
		})
	}
}

func TestParseRootLine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		line string
		link string
		ok   bool
	}{
		{
			name: "workspace root",
			line: `"/srv/example/workspaces/s/w/.devenv/gc/shell" -> /nix/store/x-shell`,
			link: "/srv/example/workspaces/s/w/.devenv/gc/shell",
			ok:   true,
		},
		{
			name: "escaped quote",
			line: `"/tmp/a\"b" -> /nix/store/y`,
			link: `/tmp/a"b`,
			ok:   true,
		},
		{
			name: "arrow inside path",
			line: `"/tmp/a -> b" -> /nix/store/z`,
			link: "/tmp/a -> b",
			ok:   true,
		},
		{name: "missing target", line: `"/tmp/a"`, ok: false},
		{name: "unquoted", line: `/tmp/a -> /nix/store/z`, ok: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			link, ok := parseRootLine(tc.line)
			if ok != tc.ok || link != tc.link {
				t.Fatalf("parseRootLine(%q) = (%q, %t), want (%q, %t)", tc.line, link, ok, tc.link, tc.ok)
			}
		})
	}
}

func TestRunWritesReportAndCandidatesFile(t *testing.T) {
	t.Parallel()
	tree := t.TempDir()
	workspaces := filepath.Join(tree, "workspaces")
	procRoot := filepath.Join(tree, "proc")
	output := filepath.Join(tree, "state", "candidates.txt")
	if err := os.MkdirAll(procRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	idle := makeWorkspace(t, workspaces, "seat-a", "ws-idle", 8*24*time.Hour)
	gcRoots := func() ([]string, error) {
		return []string{rootLine(filepath.Join(idle, "result"))}, nil
	}
	env := runEnv{
		workspaces: workspaces,
		procRoot:   procRoot,
		output:     output,
		now:        testNow,
		gcRoots:    gcRoots,
	}

	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr, env); code != 0 {
		t.Fatalf("run exit = %d, want 0; stderr:\n%s", code, stderr.String())
	}
	report := stdout.String()
	firstLine := strings.SplitN(report, "\n", 2)[0]
	wantFirst := "fleet-gc-roots: 1 idle workspace GC root(s) in 1 workspace(s); dry run, nothing deleted"
	if firstLine != wantFirst {
		t.Fatalf("summary line = %q, want %q", firstLine, wantFirst)
	}
	if !strings.HasSuffix(report, filepath.Join(idle, "result")+"\n") {
		t.Fatalf("report does not end with the candidate root:\n%s", report)
	}
	written, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != report {
		t.Fatalf("candidates file = %q, want the stdout report %q", string(written), report)
	}
}

func TestRunRejectsExtraArguments(t *testing.T) {
	t.Parallel()
	tree := t.TempDir()
	output := filepath.Join(tree, "candidates.txt")
	env := runEnv{
		workspaces: filepath.Join(tree, "workspaces"),
		procRoot:   filepath.Join(tree, "proc"),
		output:     output,
		now:        testNow,
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"extra"}, &stdout, &stderr, env); code != 2 {
		t.Fatalf("run exit = %d, want 2", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want nothing", stdout.String())
	}
	if !strings.Contains(stderr.String(), "takes no arguments") {
		t.Fatalf("stderr = %q, want the argument error", stderr.String())
	}
	if _, err := os.Stat(output); err == nil {
		t.Fatal("candidates file was written for rejected arguments")
	}
}

func TestRunFromEnvironmentRequiresBothPaths(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		env        map[string]string
		wantStderr string
	}{
		{
			name:       "missing workspaces",
			env:        map[string]string{envOutput: "/srv/example/state/candidates.txt"},
			wantStderr: envWorkspaces,
		},
		{
			name:       "missing output",
			env:        map[string]string{envWorkspaces: "/srv/example/workspaces"},
			wantStderr: envOutput,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			code := runFromEnvironment(nil, &stdout, &stderr, func(name string) string { return tc.env[name] })
			if code != 2 {
				t.Fatalf("run exit = %d, want 2; stderr:\n%s", code, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want nothing", stdout.String())
			}
			if !strings.Contains(stderr.String(), "fleet-gc-roots requires environment variables") || !strings.Contains(stderr.String(), "missing="+tc.wantStderr+"\n") {
				t.Fatalf("stderr = %q, want only the missing variable %s named", stderr.String(), tc.wantStderr)
			}
		})
	}
}
