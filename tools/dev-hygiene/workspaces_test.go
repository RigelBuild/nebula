package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// jjFixture builds a primary jj checkout backed by a bare git remote, all under t.TempDir.
// Every jj call uses a private JJ_CONFIG, so the test never reads the host's identity.
type jjFixture struct {
	t       *testing.T
	primary string
	root    string
}

func newJJFixture(t *testing.T) *jjFixture {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	config := filepath.Join(home, "jj.toml")
	mustWrite(t, config, "[user]\nname = 'example'\nemail = 'example@example.com'\n")
	t.Setenv("JJ_CONFIG", config)
	f := &jjFixture{t: t, primary: filepath.Join(home, "agents", "workspaces", "primary"), root: filepath.Join(home, "agents", "workspaces")}
	mustMkdir(t, f.primary)
	f.jj(f.primary, "git", "init", "--colocate")
	// .gitignore is tracked, so the ignored-file case below isolates the untracked scan.
	mustWrite(t, filepath.Join(f.primary, ".gitignore"), "secret.env\n")
	mustWrite(t, filepath.Join(f.primary, "README.md"), "initial\n")
	f.jj(f.primary, "describe", "-m", "chore: initial")
	f.jj(f.primary, "bookmark", "create", "main")
	bare := filepath.Join(home, "remote.git")
	runIn(t, home, "git", "init", "--bare", bare)
	runIn(t, home, "git", "--git-dir", bare, "symbolic-ref", "HEAD", "refs/heads/main")
	f.jj(f.primary, "git", "remote", "add", "origin", bare)
	f.jj(f.primary, "git", "push", "--bookmark", "main")
	f.jj(f.primary, "new", "main")
	return f
}

// secondary adds a jj workspace at root/<name> on the current main tip.
func (f *jjFixture) secondary(name string) string {
	f.t.Helper()
	dir := filepath.Join(f.root, name)
	mustMkdir(f.t, f.root)
	f.jj(f.primary, "workspace", "add", dir, "--name", name, "-r", "main")
	return dir
}

// jj runs one jj command with the same flags the tool uses, and fails the test on error.
func (f *jjFixture) jj(dir string, args ...string) string {
	f.t.Helper()
	out, err := commandRun(f.t.Context(), dir, nil, "jj", append([]string{"--no-pager", "--color=never"}, args...)...)
	if err != nil {
		f.t.Fatalf("jj %v in %s: %v", args, dir, err)
	}
	return out.stdout
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}

func runIn(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	if _, err := commandRun(t.Context(), dir, nil, name, args...); err != nil {
		t.Fatal(err)
	}
}

// backdate sets every entry under dir to at, so the activity clock reads as idle.
func backdate(t *testing.T, dir string, at time.Time) {
	t.Helper()
	if err := filepath.WalkDir(dir, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(path, at, at)
	}); err != nil {
		t.Fatal(err)
	}
}

// sweepConfig is a run with a two-day idle window and no cache or report side effects.
func sweepConfig(f *jjFixture, apply bool) config {
	return config{roots: []string{f.root}, idle: 48 * time.Hour, apply: apply, cotalBin: "cotal", to: "nemo"}
}

// lastSweepLog holds the verdict lines of the most recent sweep. The package's real-jj tests run sequentially,
// so one slot is enough; assertion messages print it, so a red run explains itself without the test log.
var lastSweepLog string

// sweep runs cleanWorkspaces with the given live cwds. Verdict reasons go to the test log and to lastSweepLog.
func sweep(t *testing.T, cfg config, live []string) []kept {
	t.Helper()
	var verdicts strings.Builder
	out := &printer{w: &bytes.Buffer{}, debug: func(format string, args ...any) {
		t.Logf(format, args...)
		fmt.Fprintf(&verdicts, format, args...)
	}}
	kept, err := cleanWorkspaces(t.Context(), cfg, live, out)
	lastSweepLog = verdicts.String()
	if err != nil {
		t.Fatalf("sweep error: %v\nverdicts:\n%s", err, lastSweepLog)
	}
	return kept
}

// TestCleanWorkspacesRealJJ builds every case on one shared primary and runs one apply sweep over the root. Each
// case then asserts its own directory: gone or kept, reported or not. One fixture keeps the package under the timeout.
func TestCleanWorkspacesRealJJ(t *testing.T) {
	ancient := time.Now().Add(-100 * time.Hour)
	f := newJJFixture(t)
	cases := []struct {
		name           string
		setup          func(t *testing.T, f *jjFixture) string
		wantGone       bool
		wantReport     bool
		wantMarkerGone bool
	}{
		{
			name: "clean",
			setup: func(t *testing.T, f *jjFixture) string {
				dir := f.secondary("clean")
				backdate(t, dir, ancient)
				return dir
			},
			wantGone: true,
		},
		{
			name: "unpushed",
			setup: func(t *testing.T, f *jjFixture) string {
				dir := f.secondary("unpushed")
				mustWrite(t, filepath.Join(dir, "work.txt"), "local only\n")
				f.jj(dir, "describe", "-m", "feat: local only")
				backdate(t, dir, ancient)
				return dir
			},
			wantReport: true,
		},
		{
			name: "ignored",
			setup: func(t *testing.T, f *jjFixture) string {
				dir := f.secondary("ignored")
				mustWrite(t, filepath.Join(dir, "secret.env"), "TOKEN=1\n")
				backdate(t, dir, ancient)
				return dir
			},
			wantReport: true,
		},
		{
			name: "oversize",
			setup: func(t *testing.T, f *jjFixture) string {
				dir := f.secondary("oversize")
				mustWrite(t, filepath.Join(dir, "huge.log"), string(bytes.Repeat([]byte("x"), 2<<20)))
				backdate(t, dir, ancient)
				return dir
			},
			wantReport: true,
		},
		{
			name: "nested",
			setup: func(t *testing.T, f *jjFixture) string {
				dir := f.secondary("nested")
				mustMkdir(t, filepath.Join(dir, "inner", ".jj"))
				backdate(t, dir, ancient)
				return dir
			},
			wantReport: true,
		},
		{
			name: "regenerable",
			setup: func(t *testing.T, f *jjFixture) string {
				dir := f.secondary("regenerable")
				// The real repos gitignore tool state; without that, jj would snapshot it as work.
				mustWrite(t, filepath.Join(f.primary, ".git", "info", "exclude"), ".devenv/\n.moon/cache/\n/target/\n/dist/\napps/web/dist/\n")
				mustWrite(t, filepath.Join(dir, ".devenv", "state"), "cache\n")
				mustWrite(t, filepath.Join(dir, ".moon", "cache", "x"), "cache\n")
				// Top-level build output is untracked and must not block deletion or defer the clock.
				mustWrite(t, filepath.Join(dir, "target", "debug", "app"), "binary\n")
				mustWrite(t, filepath.Join(dir, "dist", "bundle.js"), "built\n")
				// A nested build tree is ignored and untracked, so it is regenerable at any depth (M3).
				mustWrite(t, filepath.Join(dir, "apps", "web", "dist", "bundle.js"), "built\n")
				backdate(t, dir, ancient)
				return dir
			},
			wantGone: true,
		},
		{
			// The forget never ran: name still registered, unpushed work. The stale marker is dropped and the
			// workspace is kept and reported in the same run.
			name: "marker-registered",
			setup: func(t *testing.T, f *jjFixture) string {
				dir := f.secondary("marker-registered")
				mustWrite(t, filepath.Join(dir, "work.txt"), "local only\n")
				f.jj(dir, "describe", "-m", "feat: local only")
				mustWrite(t, filepath.Join(dir, workspaceMarker), f.primary+"\tmarker-registered\n")
				backdate(t, dir, ancient)
				return dir
			},
			wantMarkerGone: true,
			wantReport:     true,
		},
		{
			// The forget ran before a crash: name already gone from the store, so the leftover is deleted.
			name: "marker-forgotten",
			setup: func(t *testing.T, f *jjFixture) string {
				dir := f.secondary("marker-forgotten")
				f.jj(f.primary, "workspace", "forget", "marker-forgotten")
				mustWrite(t, filepath.Join(dir, workspaceMarker), f.primary+"\tmarker-forgotten\n")
				backdate(t, dir, ancient)
				return dir
			},
			wantGone: true,
		},
		{
			// A trash dir a crash left behind: its name is still registered, so the next run forgets it and removes it.
			name: "trash-leftover",
			setup: func(t *testing.T, f *jjFixture) string {
				dir := f.secondary("trash-leftover")
				trash := filepath.Join(f.root, trashPrefix+"trash-leftover")
				if err := os.Rename(dir, trash); err != nil {
					t.Fatal(err)
				}
				mustWrite(t, filepath.Join(trash, workspaceMarker), f.primary+"\ttrash-leftover\n")
				backdate(t, trash, ancient)
				return trash
			},
			wantGone: true,
		},
		{
			name: "recent",
			setup: func(t *testing.T, f *jjFixture) string {
				return f.secondary("recent")
			},
		},
		{
			// A marker snapshotted in from a forgotten ws-old: this live, rooted ws-new must be kept and reported.
			name: "marker-naming-forgotten",
			setup: func(t *testing.T, f *jjFixture) string {
				f.secondary("ws-old")
				f.jj(f.primary, "workspace", "forget", "ws-old")
				dir := f.secondary("ws-new")
				mustWrite(t, filepath.Join(dir, "work.txt"), "local only\n")
				f.jj(dir, "describe", "-m", "feat: local only")
				mustWrite(t, filepath.Join(dir, workspaceMarker), f.primary+"\tws-old\n")
				backdate(t, dir, ancient)
				return dir
			},
			wantReport: true,
		},
	}
	dirs := make([]string, len(cases))
	for i, tc := range cases {
		dirs[i] = tc.setup(t, f)
	}
	// Old and clean, but a process sits inside: the shared sweep must keep it.
	liveDir := f.secondary("live")
	backdate(t, liveDir, ancient)
	liveResolved, err := filepath.EvalSymlinks(liveDir)
	if err != nil {
		t.Fatal(err)
	}
	primaryBefore := f.jj(f.primary, "log", "--no-graph", "-r", "@", "-T", "change_id.short()")
	kept := sweep(t, sweepConfig(f, true), []string{liveResolved})
	if _, err := os.Stat(liveDir); err != nil {
		t.Fatalf("live workspace removed by the shared sweep: %v\nverdicts:\n%s", err, lastSweepLog)
	}
	primaryAfter := f.jj(f.primary, "log", "--no-graph", "-r", "@", "-T", "change_id.short()")
	if primaryAfter != primaryBefore {
		t.Fatalf("primary working copy moved: %q -> %q\nverdicts:\n%s", primaryBefore, primaryAfter, lastSweepLog)
	}
	if _, err := os.Stat(filepath.Join(f.primary, ".jj", "repo")); err != nil {
		t.Fatalf("primary store gone after the sweep: %v\nverdicts:\n%s", err, lastSweepLog)
	}
	for i, tc := range cases {
		_, statErr := os.Stat(dirs[i])
		if gone := os.IsNotExist(statErr); gone != tc.wantGone {
			t.Errorf("%s: workspace gone = %t, want %t (stat: %v; kept: %+v)\nverdicts:\n%s", tc.name, gone, tc.wantGone, statErr, kept, lastSweepLog)
		}
		if got := keptFor(kept, dirs[i]); got != tc.wantReport {
			t.Errorf("%s: reported = %t, want %t (kept %+v)\nverdicts:\n%s", tc.name, got, tc.wantReport, kept, lastSweepLog)
		}
		if tc.wantMarkerGone {
			if _, err := os.Stat(filepath.Join(dirs[i], workspaceMarker)); !os.IsNotExist(err) {
				t.Errorf("%s: stale marker still present after the sweep: stat err = %v", tc.name, err)
			}
		}
	}
}

// keptFor reports whether a sweep kept the workspace at dir.
func keptFor(kepts []kept, dir string) bool {
	for _, k := range kepts {
		if k.root == dir {
			return true
		}
	}
	return false
}

// TestDeleteReleasesWorktree checks a deleted workspace leaves no jj name and no git worktree entry, so the same
// path can be added again. Before the forget ran first, git kept a stale "missing but registered" worktree here.
func TestDeleteReleasesWorktree(t *testing.T) {
	f := newJJFixture(t)
	dir := f.secondary("reuse")
	if _, err := os.Stat(filepath.Join(f.primary, ".git", "worktrees", "reuse")); err != nil {
		t.Fatalf("setup did not register a git worktree; the absence check below would prove nothing: %v", err)
	}
	backdate(t, dir, time.Now().Add(-100*time.Hour))
	sweep(t, sweepConfig(f, true), nil)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("workspace not deleted: stat err = %v\nverdicts:\n%s", err, lastSweepLog)
	}
	if names := f.jj(f.primary, "workspace", "list", "-T", `name ++ "\n"`); strings.Contains(names, "reuse") {
		t.Fatalf("jj still lists the forgotten name: %q", names)
	}
	if _, err := os.Stat(filepath.Join(f.primary, ".git", "worktrees", "reuse")); !os.IsNotExist(err) {
		t.Fatalf("git worktree entry survives the delete: stat err = %v", err)
	}
	f.jj(f.primary, "workspace", "add", dir, "--name", "reuse", "-r", "main")
}

// TestStaleMarkerReclassifiedOnSecondSweep checks a stale marker is dropped on the first run and the workspace is
// judged normally on the next. A workspace with unpushed work must be kept and reported, never deleted.
func TestStaleMarkerReclassifiedOnSecondSweep(t *testing.T) {
	ancient := time.Now().Add(-100 * time.Hour)
	f := newJJFixture(t)
	dir := f.secondary("stale")
	mustWrite(t, filepath.Join(dir, "work.txt"), "local only\n")
	f.jj(dir, "describe", "-m", "feat: local only")
	mustWrite(t, filepath.Join(dir, workspaceMarker), f.primary+"\tstale\n")
	backdate(t, dir, ancient)
	sweep(t, sweepConfig(f, true), nil)
	if _, err := os.Stat(filepath.Join(dir, workspaceMarker)); !os.IsNotExist(err) {
		t.Fatalf("stale marker not removed on the first sweep: stat err = %v\nverdicts (first sweep):\n%s", err, lastSweepLog)
	}
	backdate(t, dir, ancient)
	kept := sweep(t, sweepConfig(f, true), nil)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("unpushed workspace removed on the second sweep: %v\nverdicts (second sweep):\n%s", err, lastSweepLog)
	}
	if !keptFor(kept, dir) {
		t.Fatalf("unpushed workspace not reported on the second sweep: kept %+v\nverdicts (second sweep):\n%s", kept, lastSweepLog)
	}
}

// TestDryRunDeletesNothing checks the default mode removes nothing. It has its own fixture: a dry run snapshots the
// working copy, so it must never share a tree with the apply sweep under test.
func TestDryRunDeletesNothing(t *testing.T) {
	ancient := time.Now().Add(-100 * time.Hour)
	f := newJJFixture(t)
	dir := f.secondary("dry")
	mustWrite(t, filepath.Join(dir, "work.txt"), "local only\n")
	backdate(t, dir, ancient)
	kept := sweep(t, sweepConfig(f, false), nil)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dry run removed the workspace: %v (kept %+v)\nverdicts:\n%s", err, kept, lastSweepLog)
	}
}

// TestInspectionKeepsIdleClock checks the sweep's own jj snapshot does not count as activity. Without the restore in
// inspectWorkspace, the .jj/working_copy mtime moves to now and the workspace reads as recent for 48h.
func TestInspectionKeepsIdleClock(t *testing.T) {
	ancient := time.Now().Add(-100 * time.Hour)
	f := newJJFixture(t)
	dir := f.secondary("clock")
	mustWrite(t, filepath.Join(dir, "work.txt"), "local only\n")
	backdate(t, dir, ancient)
	sweep(t, sweepConfig(f, false), nil)
	got, err := newestMtime(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.After(ancient.Add(time.Hour)) {
		t.Fatalf("inspection moved the activity clock to %s\nverdicts:\n%s", got, lastSweepLog)
	}
}
