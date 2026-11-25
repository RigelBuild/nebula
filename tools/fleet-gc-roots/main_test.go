// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"bytes"
	"errors"
	"log/slog"
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

func makeRootLink(t *testing.T, link, target string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func assertSymlink(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat(%q) error = %v, want symlink", path, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("Lstat(%q) mode = %v, want symlink", path, info.Mode())
	}
}

type runFixture struct {
	tree, workspaces, procRoot, output string
}

func newRunFixture(t *testing.T) runFixture {
	t.Helper()
	tree := t.TempDir()
	fixture := runFixture{
		tree:       tree,
		workspaces: filepath.Join(tree, "workspaces"),
		procRoot:   filepath.Join(tree, "proc"),
		output:     filepath.Join(tree, "state", "candidates.txt"),
	}
	if err := os.MkdirAll(fixture.procRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f runFixture) run(args []string, roots ...string) (int, string, string) {
	lines := make([]string, len(roots))
	for i, root := range roots {
		lines[i] = rootLine(root)
	}
	var stdout, stderr bytes.Buffer
	env := runEnv{
		workspaces: f.workspaces,
		procRoot:   f.procRoot,
		output:     f.output,
		now:        testNow,
		gcRoots:    func() ([]string, error) { return lines, nil },
	}
	code := run(args, &stdout, &stderr, env)
	return code, stdout.String(), stderr.String()
}

func setOld(t *testing.T, path string) {
	t.Helper()
	old := testNow.Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

func setRootParentsOld(t *testing.T, workspace, root string) {
	t.Helper()
	for dir := filepath.Dir(root); ; dir = filepath.Dir(dir) {
		setOld(t, dir)
		if dir == workspace {
			return
		}
	}
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
	wantFirst := "fleet-gc-roots: 7 idle workspace GC root(s) in 1 workspace(s); dry run, removed 0, failed 0"
	if firstLine != wantFirst {
		t.Fatalf("summary line = %q, want %q", firstLine, wantFirst)
	}
	if !strings.Contains(report, filepath.Join(idle, "result")) {
		t.Fatalf("report does not list candidate root:\n%s", report)
	}
}

func TestRunDryRunLeavesCandidateLinks(t *testing.T) {
	t.Parallel()
	fixture := newRunFixture(t)
	idle := makeWorkspace(t, fixture.workspaces, "seat-a", "ws-idle", 8*24*time.Hour)
	link, target := filepath.Join(idle, "result"), filepath.Join(fixture.tree, "target")
	makeRootLink(t, link, target)
	setRootParentsOld(t, idle, link)
	code, report, _ := fixture.run(nil, link)
	if code != 0 {
		t.Fatalf("run exit = %d, want 0", code)
	}
	assertSymlink(t, link)
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("candidate target stat error = %v, want nil", err)
	}
	if !strings.Contains(report, "dry run, removed 0, failed 0") {
		t.Fatalf("report does not identify dry run:\n%s", report)
	}
}

func TestRunApplyRemovesExactlyReportedIdleLinksAndKeepsActiveLink(t *testing.T) {
	t.Parallel()
	fixture := newRunFixture(t)
	idle := makeWorkspace(t, fixture.workspaces, "seat-a", "ws-idle", 8*24*time.Hour)
	active := makeWorkspace(t, fixture.workspaces, "ws-active", 24*time.Hour)
	busy := makeWorkspace(t, fixture.workspaces, "seat-b", "ws-busy", 8*24*time.Hour)
	idleLinks := []string{
		filepath.Join(idle, "result"),
		filepath.Join(idle, "result-1"),
	}
	nonCandidate := filepath.Join(idle, "src", "main-link")
	activeRoot := filepath.Join(active, "result")
	busyRoot := filepath.Join(busy, "result")
	links := append(append([]string{}, idleLinks...), nonCandidate, activeRoot, busyRoot)
	for i, link := range links {
		makeRootLink(t, link, filepath.Join(fixture.tree, "target-"+strconv.Itoa(i)))
	}
	for _, link := range idleLinks {
		setRootParentsOld(t, idle, link)
	}
	setOld(t, filepath.Join(idle, "src"))
	setOld(t, busy)
	process := filepath.Join(fixture.procRoot, "4242")
	if err := os.Mkdir(process, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(busy, filepath.Join(process, "cwd")); err != nil {
		t.Fatal(err)
	}

	code, report, logs := fixture.run([]string{"--apply"}, links...)
	if code != 0 {
		t.Fatalf("run exit = %d, want 0; stderr:\n%s", code, logs)
	}
	for _, link := range idleLinks {
		if _, err := os.Lstat(link); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("candidate Lstat(%q) error = %v, want not exist", link, err)
		}
	}
	assertSymlink(t, nonCandidate)
	assertSymlink(t, activeRoot)
	assertSymlink(t, busyRoot)
	if !strings.Contains(report, "apply, removed 2, failed 0") {
		t.Fatalf("report does not show exactly two removals:\n%s", report)
	}
}

func TestRemoveCandidatesSkipsWorkspaceThatBecomesBusy(t *testing.T) {
	t.Parallel()
	fixture := newRunFixture(t)
	workspace := makeWorkspace(t, fixture.workspaces, "seat-a", "ws-idle", 8*24*time.Hour)
	root := filepath.Join(workspace, "result")
	makeRootLink(t, root, filepath.Join(fixture.tree, "target"))
	setRootParentsOld(t, workspace, root)
	found, err := findCandidates(fixture.workspaces, fixture.procRoot, []string{rootLine(root)}, testNow, testIdle)
	if err != nil || len(found.Roots) != 1 {
		t.Fatalf("findCandidates = %v, %v", found.Roots, err)
	}
	process := filepath.Join(fixture.procRoot, "4242")
	if err := os.Mkdir(process, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(workspace, filepath.Join(process, "cwd")); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	env := runEnv{workspaces: fixture.workspaces, procRoot: fixture.procRoot, now: testNow}
	removed, failed := removeCandidates(found.Roots, env, slog.New(slog.NewTextHandler(&logs, nil)))
	if removed != 0 || failed != 0 {
		t.Fatalf("removed %d, failed %d; logs %s", removed, failed, logs.String())
	}
	assertSymlink(t, root)
}

func TestRunApplySkipsSymlinkedParent(t *testing.T) {
	t.Parallel()
	fixture := newRunFixture(t)
	idle := makeWorkspace(t, fixture.workspaces, "seat-a", "ws-idle", 8*24*time.Hour)
	busy := makeWorkspace(t, fixture.workspaces, "seat-a", "ws-busy", 8*24*time.Hour)
	targetRoot := filepath.Join(busy, "result")
	makeRootLink(t, targetRoot, filepath.Join(fixture.tree, "target"))
	middle := filepath.Join(idle, "middle")
	if err := os.Symlink(busy, middle); err != nil {
		t.Fatal(err)
	}
	setRootParentsOld(t, busy, targetRoot)
	setOld(t, idle)
	process := filepath.Join(fixture.procRoot, "4242")
	if err := os.Mkdir(process, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(busy, filepath.Join(process, "cwd")); err != nil {
		t.Fatal(err)
	}
	code, report, logs := fixture.run([]string{"--apply"}, filepath.Join(middle, "result"))
	if code != 0 || !strings.Contains(report, "removed 0, failed 0") {
		t.Fatalf("run = %d, report %q, logs %q", code, report, logs)
	}
	assertSymlink(t, middle)
	assertSymlink(t, targetRoot)
}

func TestRunApplySkipsReplacedNonSymlinkCandidates(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"regular file", "directory"} {
		kind := kind
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			fixture := newRunFixture(t)
			idle := makeWorkspace(t, fixture.workspaces, "seat-a", "ws-idle", 8*24*time.Hour)
			link := filepath.Join(idle, "result")
			makeRootLink(t, link, filepath.Join(fixture.tree, "target"))
			setRootParentsOld(t, idle, link)
			var stdout, stderr bytes.Buffer
			env := runEnv{
				workspaces: fixture.workspaces,
				procRoot:   fixture.procRoot,
				output:     fixture.output,
				now:        testNow,
				gcRoots: func() ([]string, error) {
					if err := os.Remove(link); err != nil {
						return nil, err
					}
					if kind == "regular file" {
						if err := os.WriteFile(link, []byte("keep"), 0o600); err != nil {
							return nil, err
						}
					} else if err := os.Mkdir(link, 0o750); err != nil {
						return nil, err
					}
					setRootParentsOld(t, idle, link)
					return []string{rootLine(link)}, nil
				},
			}
			if code := run([]string{"--apply"}, &stdout, &stderr, env); code != 0 {
				t.Fatalf("run exit = %d, want 0; stderr:\n%s", code, stderr.String())
			}
			info, err := os.Lstat(link)
			if err != nil || info.Mode()&os.ModeSymlink != 0 {
				t.Fatalf("replacement Lstat = (%v, %v), want existing non-symlink", info, err)
			}
			if !strings.Contains(stdout.String(), "apply, removed 0, failed 0") {
				t.Fatalf("report has wrong skip counts:\n%s", stdout.String())
			}
		})
	}
}

func TestRunApplyReportsRemovalFailure(t *testing.T) {
	t.Parallel()
	fixture := newRunFixture(t)
	idle := makeWorkspace(t, fixture.workspaces, "seat-a", "ws-idle", 8*24*time.Hour)
	root := filepath.Join(idle, "result-"+strings.Repeat("x", 256))
	code, report, _ := fixture.run([]string{"--apply"}, root)
	if code != 1 || !strings.Contains(report, "removed 0, failed 1") {
		t.Fatalf("run = %d, report %q", code, report)
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
	wantFirst := "fleet-gc-roots: 1 idle workspace GC root(s) in 1 workspace(s); dry run, removed 0, failed 0"
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
	if !strings.Contains(stderr.String(), "takes no arguments other than --apply") {
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
