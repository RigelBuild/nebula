package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fleetMaxIdle is the idle age the fleet home prune removes at.
const fleetMaxIdle = 72 * time.Hour

// homeOld is an age past fleetMaxIdle.
var homeOld = testNow.Add(-5 * 24 * time.Hour)

// homeOpts returns home prune options with the real cwd reader and the given process table.
func homeOpts(t *testing.T, home, procDir string) options {
	t.Helper()
	return options{root: resolvedRoot(t, home), home: true, maxIdle: fleetMaxIdle, procDir: procDir, readCwd: os.Readlink}
}

// touchAt creates an empty file at path with the given mtime.
func touchAt(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	setMtime(t, path, mtime)
}

// setMtime sets path's mtime. Set it after a directory's children change, which bumps the directory's own mtime.
func setMtime(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// idleTree creates home/rel with a file and every dir along the way set to homeOld.
func idleTree(t *testing.T, home, rel string) string {
	t.Helper()
	path := filepath.Join(home, rel)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	touchAt(t, filepath.Join(path, "data"), homeOld)
	for p := path; p != home; p = filepath.Dir(p) {
		setMtime(t, p, homeOld)
	}
	return path
}

func pruneHome(t *testing.T, opts options) string {
	t.Helper()
	var out strings.Builder
	if err := prune(opts, testNow, &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestHomePruneKeepsSharedInfrastructure(t *testing.T) {
	cases := []string{
		"rjc-hook/example/app.git",
		"rjc-hook/example/app",
		"rjc-hook/tools/hk-1.0.0-x86_64-linux",
		"rjc-hook/slots",
		"rjc-hook/queue",
		".cache/go-build",
		".cache/nix",
		".cache/golangci-lint",
		".cache/staticcheck",
		"rbuild/.locks",
		"rbuild/seat/ws",
		"go/pkg",
		"hermes/x",
		"agents/x",
		".config/x",
	}
	for _, rel := range cases {
		t.Run(rel, func(t *testing.T) {
			home := t.TempDir()
			path := idleTree(t, home, rel)
			pruneHome(t, homeOpts(t, home, makeProcTable(t)))
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("%s removed: %v", rel, err)
			}
		})
	}
}

func TestHomePruneRemovesIdlePerJobDirs(t *testing.T) {
	cases := []string{
		"web-old",
		"harness/old-run",
		"rjc-hook/tmp/rjc-hook.abc123",
		".cache/10602.IF815A",
		"work/job",
		"tmp/job",
	}
	for _, rel := range cases {
		t.Run(rel, func(t *testing.T) {
			home := t.TempDir()
			path := idleTree(t, home, rel)
			pruneHome(t, homeOpts(t, home, makeProcTable(t)))
			if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("%s survived the prune: %v", rel, err)
			}
		})
	}
}

// TestHomeIdleSeesFreshFileAnywhereInSubtree keeps a dir whose only fresh file is deep inside it.
func TestHomeIdleSeesFreshFileAnywhereInSubtree(t *testing.T) {
	cases := []struct {
		name string
		deep string
	}{
		{"direct", "out.log"},
		{"deep", "a/b/c/out.log"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			job := idleTree(t, home, filepath.Join("harness", "job", filepath.Dir(tc.deep)))
			touchAt(t, filepath.Join(job, filepath.Base(tc.deep)), testNow.Add(-time.Hour))
			for p := job; p != home; p = filepath.Dir(p) {
				setMtime(t, p, homeOld)
			}
			pruneHome(t, homeOpts(t, home, makeProcTable(t)))
			if _, err := os.Stat(filepath.Join(home, "harness", "job")); err != nil {
				t.Fatalf("dir with a fresh %s file removed: %v", tc.name, err)
			}
		})
	}
}

// steppingClock returns a clock that moves one second forward on every read.
func steppingClock() func() time.Time {
	tick := testNow
	return func() time.Time {
		tick = tick.Add(time.Second)
		return tick
	}
}

// TestHomeIdleWalkBoundFallsBackToTopLevel judges a tree past the time or depth bound
// by its top-level entries: idle ones are pruned, one fresh top-level entry keeps it.
func TestHomeIdleWalkBoundFallsBackToTopLevel(t *testing.T) {
	deep := "harness/job"
	for range idleWalkMaxDepth + 1 {
		deep += "/d"
	}
	cases := []struct {
		name      string
		tree      string
		freshTop  bool
		timeBound bool
		removed   bool
	}{
		{"time bound idle", "harness/job/a", false, true, false},
		{"time bound fresh top-level entry", "harness/job/a", true, true, false},
		{"depth bound idle", deep, false, false, false},
		{"depth bound fresh top-level entry", deep, true, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			idleTree(t, home, tc.tree)
			job := filepath.Join(home, "harness", "job")
			if tc.freshTop {
				touchAt(t, filepath.Join(job, "out.log"), testNow.Add(-time.Hour))
				setMtime(t, job, homeOld)
			}
			opts := homeOpts(t, home, makeProcTable(t))
			if tc.timeBound {
				opts.walk = homeWalk{budget: time.Nanosecond, clock: steppingClock()}
			}
			out := pruneHome(t, opts)
			if !strings.Contains(out, "walk bound reached in harness/job") {
				t.Fatalf("output %q does not report the walk bound", out)
			}
			_, err := os.Stat(job)
			if gone := errors.Is(err, fs.ErrNotExist); gone != tc.removed {
				t.Fatalf("job removed = %v, want %v (stat err %v)", gone, tc.removed, err)
			}
		})
	}
}

func TestHomePruneDryRunRemovesNothing(t *testing.T) {
	home := t.TempDir()
	job := idleTree(t, home, "harness/old-run")
	top := idleTree(t, home, "web-old")
	leftover := idleTree(t, home, "harness/.prune-old-run-42")
	opts := homeOpts(t, home, makeProcTable(t))
	opts.dryRun = true
	out := pruneHome(t, opts)
	for _, path := range []string{job, top, leftover} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("dry run removed %s: %v", path, err)
		}
	}
	if !strings.Contains(out, "would remove harness/old-run") {
		t.Fatalf("output %q does not report the dry-run candidate", out)
	}
}

func TestHomePruneKeepsDirWithBusyCwdOrOpenFile(t *testing.T) {
	home := t.TempDir()
	cwdBusy := setWorkspace(t, home, "", "work-cwd", homeOld)
	fdBusy := setWorkspace(t, home, "rjc-hook/tmp", "busy-fd", homeOld)
	idle := setWorkspace(t, home, "", "web-idle", homeOld)
	procDir := makeProcTable(t, "100", "200")
	linkCwd(t, procDir, "100", cwdBusy)
	// Pid 200 has no cwd link, so only its open file pins busy-fd.
	if err := os.Mkdir(filepath.Join(procDir, "200", "fd"), 0o755); err != nil {
		t.Fatal(err)
	}
	openFile := filepath.Join(resolvedRoot(t, home), "rjc-hook", "tmp", "busy-fd", "build.log")
	if err := os.Symlink(openFile, filepath.Join(procDir, "200", "fd", "3")); err != nil {
		t.Fatal(err)
	}

	out := pruneHome(t, homeOpts(t, home, procDir))
	for _, busy := range []string{cwdBusy, fdBusy} {
		if _, err := os.Stat(busy); err != nil {
			t.Fatalf("busy dir removed: %v", err)
		}
	}
	if _, err := os.Stat(idle); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("idle dir survived: %v", err)
	}
	if !strings.Contains(out, "skip busy rjc-hook/tmp/busy-fd") {
		t.Fatalf("output %q does not report the open-file skip", out)
	}
}

func TestHomePruneHandlesStagedLeftovers(t *testing.T) {
	home := t.TempDir()
	idleLeftover := setWorkspace(t, home, "harness", ".prune-old-run-42", homeOld)
	recentLeftover := setWorkspace(t, home, "harness", ".prune-new-run-7", testNow.Add(-time.Hour))
	pruneHome(t, homeOpts(t, home, makeProcTable(t)))
	if _, err := os.Stat(idleLeftover); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("idle leftover survived: %v", err)
	}
	if _, err := os.Stat(recentLeftover); err != nil {
		t.Fatalf("recent leftover removed: %v", err)
	}
}

func TestRunHomeRemovesIdleTopLevelEntry(t *testing.T) {
	home := t.TempDir()
	idle := setWorkspace(t, home, "", "web-idle", homeOld)
	var stdout, stderr strings.Builder
	args := []string{"--home", home, "--max-idle", "72h", "--proc", makeProcTable(t)}
	if err := run(args, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(idle); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("run --home left the idle entry: %v", err)
	}
}

func TestRunRootAndHomeAreExclusive(t *testing.T) {
	var stdout, stderr strings.Builder
	if err := run([]string{"--root", t.TempDir(), "--home", t.TempDir()}, &stdout, &stderr); err == nil {
		t.Fatal("run accepted --root and --home together")
	}
}

func TestRunWithMissingHomeIsNoOp(t *testing.T) {
	var stdout, stderr strings.Builder
	missing := filepath.Join(t.TempDir(), "absent")
	if err := run([]string{"--home", missing}, &stdout, &stderr); err != nil {
		t.Fatalf("missing home: %v", err)
	}
}
