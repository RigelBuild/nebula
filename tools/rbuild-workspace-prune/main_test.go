package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

var testNow = time.Date(2026, time.November, 17, 12, 0, 0, 0, time.UTC)

// setWorkspace creates root/seat/name as a directory with the given mtime and
// returns its path with symlinks resolved, the form /proc reports a cwd in.
// An unresolved temp dir (macOS /tmp is a symlink) would not match the
// resolved root prune compares against.
func setWorkspace(t *testing.T, root, seat, name string, mtime time.Time) string {
	t.Helper()
	path := filepath.Join(root, seat, name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// makeProcTable creates a fake process table with one directory per name.
func makeProcTable(t *testing.T, names ...string) string {
	t.Helper()
	procDir := t.TempDir()
	for _, name := range names {
		if err := os.Mkdir(filepath.Join(procDir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return procDir
}

// linkCwd makes procDir/pid/cwd a symlink to target, as /proc does.
func linkCwd(t *testing.T, procDir, pid, target string) {
	t.Helper()
	if err := os.Symlink(target, filepath.Join(procDir, pid, "cwd")); err != nil {
		t.Fatal(err)
	}
}

// resolvedRoot returns root with symlinks resolved, as run passes it on.
func resolvedRoot(t *testing.T, root string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestIdleCandidatesSelectsOnlyWorkspacesIdleOverMaxIdle(t *testing.T) {
	root := t.TempDir()
	old := testNow.Add(-25 * time.Hour)
	setWorkspace(t, root, "seat", "old", old)
	setWorkspace(t, root, "seat", "fresh", testNow.Add(-23*time.Hour))
	setWorkspace(t, root, "seat", "boundary", testNow.Add(-24*time.Hour))
	setWorkspace(t, root, "seat", ".locks", old)
	setWorkspace(t, root, "seat", ".prune-old-42", old)
	setWorkspace(t, root, ".locks", "old", old)
	filePath := filepath.Join(root, "seat", "file")
	if err := os.WriteFile(filePath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filePath, old, old); err != nil {
		t.Fatal(err)
	}

	r := openRootForTest(t, root)
	got, err := idleCandidates(r, testNow, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join("seat", "old")}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("idleCandidates = %v, want %v", got, want)
	}
}

// TestIdleCandidatesKeepsWorkspaceWithRecentTopLevelEntry keeps a workspace
// whose dir mtime is old but whose top-level entry a run touched recently.
func TestIdleCandidatesKeepsWorkspaceWithRecentTopLevelEntry(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "seat", "hot")
	entry := filepath.Join(ws, "go.mod")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	recent := testNow.Add(-time.Hour)
	if err := os.Chtimes(entry, recent, recent); err != nil {
		t.Fatal(err)
	}
	old := testNow.Add(-48 * time.Hour)
	if err := os.Chtimes(ws, old, old); err != nil {
		t.Fatal(err)
	}

	got, err := idleCandidates(openRootForTest(t, root), testNow, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("idleCandidates = %v, want none", got)
	}
}

func TestBusyRelativeSkipsExitedProcessesAndKeepsInsideRoot(t *testing.T) {
	root := t.TempDir()
	procDir := makeProcTable(t, "100", "200", "300", "400")
	readLink := func(path string) (string, error) {
		switch pid := filepath.Base(filepath.Dir(path)); pid {
		case "100":
			return filepath.Join(root, "seat", "ws", "build"), nil
		case "200":
			return "", &fs.PathError{Op: "readlink", Path: path, Err: syscall.ENOENT}
		case "300":
			return "", &fs.PathError{Op: "readlink", Path: path, Err: syscall.ESRCH}
		case "400":
			return "/elsewhere/entirely", nil
		default:
			t.Fatalf("unexpected cwd read for %s", path)
			return "", nil
		}
	}
	busy, err := busyRelative(procDir, root, readLink)
	if err != nil {
		t.Fatal(err)
	}
	if len(busy) != 1 || busy[0] != filepath.Join("seat", "ws", "build") {
		t.Fatalf("busy = %v, want only the live in-root cwd", busy)
	}
}

func TestBusyRelativeAbortsOnUnreadableCwd(t *testing.T) {
	procDir := makeProcTable(t, "100")
	readLink := func(path string) (string, error) {
		return "", &fs.PathError{Op: "readlink", Path: path, Err: syscall.EACCES}
	}
	if _, err := busyRelative(procDir, t.TempDir(), readLink); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("err = %v, want EACCES", err)
	}
}

func TestBusyRelativeIgnoresNonPIDEntries(t *testing.T) {
	procDir := makeProcTable(t, "self", "thread-self", "100")
	calls := 0
	readLink := func(path string) (string, error) {
		calls++
		if filepath.Base(filepath.Dir(path)) != "100" {
			t.Fatalf("read cwd of non-PID entry: %s", path)
		}
		return "", &fs.PathError{Op: "readlink", Path: path, Err: syscall.ENOENT}
	}
	busy, err := busyRelative(procDir, t.TempDir(), readLink)
	if err != nil {
		t.Fatal(err)
	}
	if len(busy) != 0 || calls != 1 {
		t.Fatalf("busy = %v, calls = %d; want no busy cwds and one read", busy, calls)
	}
}

func TestIsBusyMatchesCandidateAndDescendantsOnly(t *testing.T) {
	const candidate = "seat/ws"
	cases := []struct {
		name string
		busy []string
		want bool
	}{
		{"exact cwd", []string{candidate}, true},
		{"cwd inside candidate", []string{candidate + "/build/src"}, true},
		{"sibling sharing a name prefix", []string{candidate + "2"}, false},
		{"parent of candidate", []string{"seat"}, false},
		{"no processes", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isBusy(candidate, tc.busy); got != tc.want {
				t.Fatalf("isBusy = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPruneRemovesIdleWorkspacesAndKeepsBusyOnes(t *testing.T) {
	root := t.TempDir()
	old := testNow.Add(-48 * time.Hour)
	idle := setWorkspace(t, root, "seat", "idle", old)
	busy := setWorkspace(t, root, "seat", "busy", old)
	procDir := makeProcTable(t, "100", "200")
	linkCwd(t, procDir, "100", busy)
	// Pid 200 has no cwd entry, as an exited process does; it must not abort the run.

	var out strings.Builder
	opts := options{root: resolvedRoot(t, root), maxIdle: 24 * time.Hour, procDir: procDir, readCwd: os.Readlink}
	if err := prune(opts, testNow, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(idle); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("idle workspace still present: %v", err)
	}
	if _, err := os.Stat(busy); err != nil {
		t.Fatalf("busy workspace removed: %v", err)
	}
	if !strings.Contains(out.String(), "skip busy seat/busy") {
		t.Fatalf("output %q does not report the skipped workspace", out.String())
	}
}

func TestPruneRemovesNothingWhenCwdReadFails(t *testing.T) {
	root := t.TempDir()
	idle := setWorkspace(t, root, "seat", "idle", testNow.Add(-48*time.Hour))
	procDir := makeProcTable(t, "100")
	// A real directory is not a symlink, so readlink fails with EINVAL, which is not an exited process.
	if err := os.Mkdir(filepath.Join(procDir, "100", "cwd"), 0o755); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	opts := options{root: resolvedRoot(t, root), maxIdle: 24 * time.Hour, procDir: procDir, readCwd: os.Readlink}
	if err := prune(opts, testNow, &out); err == nil {
		t.Fatal("prune succeeded with an unreadable cwd")
	}
	if _, err := os.Stat(idle); err != nil {
		t.Fatalf("idle workspace removed after cwd read failed: %v", err)
	}
}

func TestPruneDryRunReportsWithoutRemoving(t *testing.T) {
	root := t.TempDir()
	idle := setWorkspace(t, root, "seat", "idle", testNow.Add(-48*time.Hour))
	procDir := makeProcTable(t)

	var out strings.Builder
	opts := options{root: resolvedRoot(t, root), maxIdle: 24 * time.Hour, procDir: procDir, dryRun: true, readCwd: os.Readlink}
	if err := prune(opts, testNow, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(idle); err != nil {
		t.Fatalf("dry run removed the workspace: %v", err)
	}
	if !strings.Contains(out.String(), "would remove seat/idle") {
		t.Fatalf("output %q does not report the candidate", out.String())
	}
}

// TestPruneSymlinkSwapCannotDeleteOutsideRoot replaces a seat with a symlink
// to a directory outside the root between listing and deletion. The root
// handle must refuse to follow it, so the outside workspace survives.
func TestPruneSymlinkSwapCannotDeleteOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	// Named like the candidate, so the swapped seat/ws resolves to it.
	victim := filepath.Join(outside, "ws")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	setWorkspace(t, root, "seat", "ws", testNow.Add(-48*time.Hour))
	seat := filepath.Join(root, "seat")

	r := openRootForTest(t, root)
	candidates, err := idleCandidates(r, testNow, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0] != filepath.Join("seat", "ws") {
		t.Fatalf("candidates = %v, want [seat/ws]", candidates)
	}
	// Swap the seat for a symlink to outside after the listing.
	if err := os.RemoveAll(seat); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, seat); err != nil {
		t.Fatal(err)
	}
	procDir := makeProcTable(t)
	opts := options{root: resolvedRoot(t, root), maxIdle: 24 * time.Hour, procDir: procDir, readCwd: os.Readlink}
	var out strings.Builder
	if err := pruneCandidate(r, candidates[0], nil, opts, testNow, &printer{w: &out}); err == nil {
		t.Fatalf("pruneCandidate followed a symlink out of the root; output %q", out.String())
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("directory outside the root was removed: %v", err)
	}
}

// TestPruneBusyAfterStageIsRenamedBack starts a process inside the workspace
// after it has been staged. The recheck must see it and restore the name.
func TestPruneBusyAfterStageIsRenamedBack(t *testing.T) {
	root := t.TempDir()
	idle := setWorkspace(t, root, "seat", "race", testNow.Add(-48*time.Hour))
	procDir := makeProcTable(t, "100")
	calls := 0
	readLink := func(path string) (string, error) {
		calls++
		// First scan sees nothing. The rescan sees a process inside the staged name.
		if calls == 1 {
			return "", &fs.PathError{Op: "readlink", Path: path, Err: syscall.ENOENT}
		}
		staged := filepath.Join(resolvedRoot(t, root), "seat", ".prune-race-"+strconvPID())
		return filepath.Join(staged, "work"), nil
	}

	r := openRootForTest(t, root)
	var out strings.Builder
	opts := options{root: resolvedRoot(t, root), maxIdle: 24 * time.Hour, procDir: procDir, readCwd: readLink}
	candidates, err := idleCandidates(r, testNow, opts.maxIdle)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %v, want one", candidates)
	}
	busy, err := busyRelative(procDir, resolvedRoot(t, root), readLink)
	if err != nil {
		t.Fatal(err)
	}
	if err := pruneCandidate(r, candidates[0], busy, opts, testNow, &printer{w: &out}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(idle); err != nil {
		t.Fatalf("workspace not restored after busy recheck: %v", err)
	}
	if !strings.Contains(out.String(), "skip busy after stage seat/race") {
		t.Fatalf("output %q does not report the busy recheck", out.String())
	}
}

// TestPruneRecreatedNameIsNotReplaced recreates the workspace name after it was
// staged, with a process inside it. The staged copy is idle and must be deleted
// without being renamed back over the new directory.
func TestPruneRecreatedNameIsNotReplaced(t *testing.T) {
	root := t.TempDir()
	setWorkspace(t, root, "seat", "race", testNow.Add(-48*time.Hour))
	resolved := resolvedRoot(t, root)
	fresh := filepath.Join(resolved, "seat", "race")
	marker := filepath.Join(fresh, "marker")
	staged := filepath.Join(resolved, "seat", ".prune-race-"+strconvPID())
	procDir := makeProcTable(t, "100")
	calls := 0
	readLink := func(path string) (string, error) {
		calls++
		if calls == 1 {
			return "", &fs.PathError{Op: "readlink", Path: path, Err: syscall.ENOENT}
		}
		// A new run recreated the name and works inside it.
		if err := os.MkdirAll(fresh, 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(marker, nil, 0o644); err != nil {
			return "", err
		}
		return fresh, nil
	}

	r := openRootForTest(t, root)
	opts := options{root: resolved, maxIdle: 24 * time.Hour, procDir: procDir, readCwd: readLink}
	busy, err := busyRelative(procDir, resolved, readLink)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := pruneCandidate(r, filepath.Join("seat", "race"), busy, opts, testNow, &printer{w: &out}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("readLink calls = %d, want 2 (scan and rescan)", calls)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("recreated workspace was replaced: %v", err)
	}
	if _, err := os.Stat(staged); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("idle staged copy still present: %v", err)
	}
	if !strings.Contains(out.String(), "removed seat/race") {
		t.Fatalf("output %q does not report the removal under the original name", out.String())
	}
}

// TestPruneStagedAndRecreatedBothBusyLeavesBoth has one process in the staged
// copy and another in a recreated name. Neither may be deleted or renamed.
func TestPruneStagedAndRecreatedBothBusyLeavesBoth(t *testing.T) {
	root := t.TempDir()
	setWorkspace(t, root, "seat", "race", testNow.Add(-48*time.Hour))
	resolved := resolvedRoot(t, root)
	fresh := filepath.Join(resolved, "seat", "race")
	marker := filepath.Join(fresh, "marker")
	staged := filepath.Join(resolved, "seat", ".prune-race-"+strconvPID())
	procDir := makeProcTable(t, "100", "200")
	scans := 0
	readLink := func(path string) (string, error) {
		// Each scan reads pid 100 first; the rescan starts at the third call.
		if filepath.Base(filepath.Dir(path)) == "100" {
			scans++
		}
		if scans < 2 {
			return "", &fs.PathError{Op: "readlink", Path: path, Err: syscall.ENOENT}
		}
		if filepath.Base(filepath.Dir(path)) == "100" {
			return filepath.Join(staged, "work"), nil
		}
		if err := os.MkdirAll(fresh, 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(marker, nil, 0o644); err != nil {
			return "", err
		}
		return fresh, nil
	}

	r := openRootForTest(t, root)
	opts := options{root: resolved, maxIdle: 24 * time.Hour, procDir: procDir, readCwd: readLink}
	busy, err := busyRelative(procDir, resolved, readLink)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := pruneCandidate(r, filepath.Join("seat", "race"), busy, opts, testNow, &printer{w: &out}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("busy staged copy removed or renamed: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("recreated workspace was replaced: %v", err)
	}
	if !strings.Contains(out.String(), "skip busy after stage seat/race") {
		t.Fatalf("output %q does not report the busy recheck", out.String())
	}
}

// writeSlotOwner creates root/.locks/slot-N/owner with the given contents and
// sets the lock dir mtime to testNow, the clock prune is given.
func writeSlotOwner(t *testing.T, root string, slot int, owner string) {
	t.Helper()
	lock := filepath.Join(root, lockDirName, "slot-"+strconv.Itoa(slot))
	if err := os.MkdirAll(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lock, "owner"), []byte(owner), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(lock, testNow, testNow); err != nil {
		t.Fatal(err)
	}
}

func TestGlobPrune(t *testing.T) {
	cases := []struct {
		name, pattern, path, busy string
		recent, lock, removed     bool
	}{
		{"agents workspace", "workspaces/*/*", "workspaces/seat/ws", "", false, false, true},
		{"agents clone", "rigel/clones/*", "rigel/clones/x", "", false, false, true},
		{"agents factory", "rigel/fix-6766b-run/*", "rigel/fix-6766b-run/factory", "", false, false, true},
		{"rjc worktree", "RigelBuild/*/w[0-9]*", "RigelBuild/repo/w0", "", false, false, true},
		{"recent nested file", "workspaces/*/*", "workspaces/seat/ws", "", true, false, false},
		{"live cwd", "workspaces/*/*", "workspaces/seat/ws", "cwd", false, false, false},
		{"open fd", "workspaces/*/*", "workspaces/seat/ws", "fd", false, false, false},
		{"held slot", "workspaces/*/*", "workspaces/seat/ws", "", false, true, true},
		{"shared bare cache", "RigelBuild/*/w[0-9]*", "RigelBuild/x.git", "", false, false, false},
		{"shared tools", "RigelBuild/*/w[0-9]*", "tools/cache", "", false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := idleTree(t, root, tc.path)
			if tc.recent {
				touchAt(t, filepath.Join(path, "data"), testNow.Add(-time.Hour))
				setMtime(t, path, homeOld)
			}
			proc := makeProcTable(t)
			if tc.busy != "" {
				proc = makeProcTable(t, "100")
				if tc.busy == "cwd" {
					linkCwd(t, proc, "100", resolvedRoot(t, path))
				} else {
					fd := filepath.Join(proc, "100", "fd")
					if err := os.Mkdir(fd, 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(filepath.Join(resolvedRoot(t, path), "data"), filepath.Join(fd, "3")); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.lock {
				writeSlotOwner(t, root, 0, "pending held\n")
			}
			var out strings.Builder
			opts := options{root: resolvedRoot(t, root), pattern: tc.pattern, maxIdle: 24 * time.Hour, procDir: proc, readCwd: os.Readlink}
			if err := prune(opts, testNow, &out); err != nil {
				t.Fatal(err)
			}
			_, err := os.Stat(path)
			if gone := errors.Is(err, fs.ErrNotExist); gone != tc.removed {
				t.Fatalf("removed=%v want %v: %v", gone, tc.removed, err)
			}
		})
	}
}

func TestGlobStagedAndSibling(t *testing.T) {
	for _, tc := range []struct {
		name         string
		staged, busy bool
	}{
		{"staged live fd", true, true},
		{"staged idle", true, false},
		{"container live child and idle sibling", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			name := "live"
			if tc.staged {
				name = ".prune-live-123"
			}
			live := idleTree(t, root, filepath.Join("workspaces", "seat", name))
			idle := idleTree(t, root, "workspaces/seat/idle")
			proc := makeProcTable(t)
			if tc.busy {
				proc = makeProcTable(t, "100")
				fd := filepath.Join(proc, "100", "fd")
				if err := os.Mkdir(fd, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(resolvedRoot(t, live), "data"), filepath.Join(fd, "3")); err != nil {
					t.Fatal(err)
				}
			}
			var out strings.Builder
			opts := options{root: resolvedRoot(t, root), pattern: "workspaces/*/*", maxIdle: 24 * time.Hour, procDir: proc, readCwd: os.Readlink}
			if err := prune(opts, testNow, &out); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(live); errors.Is(err, fs.ErrNotExist) == tc.busy {
				t.Fatalf("live removed=%v, busy=%v", errors.Is(err, fs.ErrNotExist), tc.busy)
			}
			if _, err := os.Stat(idle); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("idle sibling kept: %v", err)
			}
		})
	}
}

// TestPruneRemovesIdleLeftoverFromCrashedRun deletes a staged directory left
// behind when a run stopped, if nothing is inside it.
func TestPruneRemovesIdleLeftoverFromCrashedRun(t *testing.T) {
	root := t.TempDir()
	leftover := setWorkspace(t, root, "seat", ".prune-old-9999", testNow.Add(-48*time.Hour))
	procDir := makeProcTable(t)

	var out strings.Builder
	opts := options{root: resolvedRoot(t, root), maxIdle: 24 * time.Hour, procDir: procDir, readCwd: os.Readlink}
	if err := prune(opts, testNow, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leftover); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("leftover still present: %v", err)
	}
	if !strings.Contains(out.String(), "removed seat/.prune-old-9999") {
		t.Fatalf("output %q does not report the leftover removal", out.String())
	}
}

// TestPruneKeepsBusyLeftover keeps a staged leftover while a process is in it.
func TestPruneKeepsBusyLeftover(t *testing.T) {
	root := t.TempDir()
	leftover := setWorkspace(t, root, "seat", ".prune-old-9999", testNow.Add(-48*time.Hour))
	procDir := makeProcTable(t, "100")
	linkCwd(t, procDir, "100", filepath.Join(leftover, "work"))

	var out strings.Builder
	opts := options{root: resolvedRoot(t, root), maxIdle: 24 * time.Hour, procDir: procDir, readCwd: os.Readlink}
	if err := prune(opts, testNow, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leftover); err != nil {
		t.Fatalf("busy leftover removed: %v", err)
	}
}

// TestPruneKeepsRecentLeftover keeps a .prune-* dir younger than maxIdle; it
// may be a live workspace that happens to use the prefix.
func TestPruneKeepsRecentLeftover(t *testing.T) {
	root := t.TempDir()
	leftover := setWorkspace(t, root, "seat", ".prune-new-9999", testNow.Add(-time.Hour))
	procDir := makeProcTable(t)

	var out strings.Builder
	opts := options{root: resolvedRoot(t, root), maxIdle: 24 * time.Hour, procDir: procDir, readCwd: os.Readlink}
	if err := prune(opts, testNow, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leftover); err != nil {
		t.Fatalf("recent leftover removed: %v", err)
	}
}

// TestPruneRemoveErrorIsReturned makes one workspace undeletable and checks
// that the error is returned and the other idle workspace is still removed.
func TestPruneRemoveErrorIsReturned(t *testing.T) {
	root := t.TempDir()
	stuck := setWorkspace(t, root, "seat", "stuck", testNow.Add(-48*time.Hour))
	idle := setWorkspace(t, root, "seat", "idle", testNow.Add(-48*time.Hour))
	// A file the workspace cannot shed: the directory is read-only, so RemoveAll cannot unlink its entries.
	if err := os.WriteFile(filepath.Join(stuck, "pinned"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// The write bumped both mtimes; age them again so stuck is still a candidate.
	for _, p := range []string{filepath.Join(stuck, "pinned"), stuck} {
		if err := os.Chtimes(p, testNow.Add(-48*time.Hour), testNow.Add(-48*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(stuck, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, p := range []string{stuck, filepath.Join(root, "seat", ".prune-stuck-"+strconvPID())} {
			_ = os.Chmod(p, 0o755)
		}
	})
	procDir := makeProcTable(t)

	var out strings.Builder
	opts := options{root: resolvedRoot(t, root), maxIdle: 24 * time.Hour, procDir: procDir, readCwd: os.Readlink}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions; remove-error path needs an unprivileged user")
	}
	err := prune(opts, testNow, &out)
	if err == nil {
		t.Fatal("prune returned nil with an undeletable workspace")
	}
	if !strings.Contains(err.Error(), "remove seat/.prune-stuck-") {
		t.Fatalf("error %q does not name the staged stuck workspace", err)
	}
	if _, statErr := os.Stat(idle); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("other idle workspace not removed: %v", statErr)
	}
}

func TestRunWithMissingRootIsNoOp(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	var stdout, stderr strings.Builder
	if err := run([]string{"--root", missing}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
}

func TestRunRejectsMissingRootAndBadArguments(t *testing.T) {
	cases := [][]string{
		{},
		{"--root", t.TempDir(), "--max-idle", "0s"},
		{"--root", t.TempDir(), "extra"},
	}
	for _, args := range cases {
		var stdout, stderr strings.Builder
		if err := run(args, &stdout, &stderr); err == nil {
			t.Fatalf("run(%q) succeeded", args)
		}
	}
}

func strconvPID() string {
	return strconv.Itoa(os.Getpid())
}

func openRootForTest(t *testing.T, root string) *os.Root {
	t.Helper()
	r, err := os.OpenRoot(resolvedRoot(t, root))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}
