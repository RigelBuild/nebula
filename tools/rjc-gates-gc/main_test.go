package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRun(t *testing.T) {
	for _, test := range []struct {
		name       string
		args       []string
		wantCode   int
		wantOutput string
	}{
		{name: "help", args: []string{"--help"}, wantOutput: "Remove expired runner worktrees"},
		{name: "unexpected positional argument", args: []string{"extra"}, wantCode: 2, wantOutput: "unknown command"},
		{name: "unknown flag", args: []string{"--unknown"}, wantCode: 2, wantOutput: "unknown flag"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(test.args, &stdout, &stderr)
			if code != test.wantCode {
				t.Fatalf("run() code = %d, want %d; stderr = %q", code, test.wantCode, stderr.String())
			}
			if !strings.Contains(stdout.String()+stderr.String(), test.wantOutput) {
				t.Fatalf("run() output = %q, want it to contain %q", stdout.String()+stderr.String(), test.wantOutput)
			}
		})
	}
	state := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--state", state}, &stdout, &stderr); code != 0 {
		t.Fatalf("run() with an empty state directory = %d; stderr = %q", code, stderr.String())
	}
}

func TestRunRequiresState(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "missing", args: []string{}},
		{name: "empty", args: []string{"--state", ""}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			t.Chdir(root)
			// An empty state would resolve work/ to this directory, so an
			// expired worktree here shows whether gc ran.
			worktree := makeWorktree(t, filepath.Join(root, "work"), "old", time.Now().Add(-15*24*time.Hour))
			var stdout, stderr bytes.Buffer
			if code := run(test.args, &stdout, &stderr); code == 0 {
				t.Fatalf("run(%q) = 0; want a non-zero exit without --state", test.args)
			}
			if !strings.Contains(stderr.String(), "state") {
				t.Fatalf("stderr = %q; want it to name --state", stderr.String())
			}
			if _, err := os.Stat(worktree); err != nil {
				t.Fatalf("worktree removed without --state: %v", err)
			}
		})
	}
}

func TestStateFlagHasNoDefault(t *testing.T) {
	var out bytes.Buffer
	command, _, err := newRootCommand(&out, &out)
	if err != nil {
		t.Fatal(err)
	}
	stateFlag := command.Flags().Lookup("state")
	if stateFlag == nil {
		t.Fatal("--state flag is not defined")
	}
	if stateFlag.DefValue != "" {
		t.Fatalf("--state default = %q; want no default", stateFlag.DefValue)
	}
}

func TestCleanWorktrees(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	old := makeWorktree(t, root, "old", now.Add(-15*24*time.Hour))
	recent := makeWorktree(t, root, "recent", now.Add(-time.Hour))
	if err := cleanWorktrees(root, now, worktreeMaxBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old worktree remains, stat error = %v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("recent worktree missing: %v", err)
	}
}

func TestCleanWorktreesSkipsHeldLock(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	worktree := makeWorktree(t, root, "held", now.Add(-15*24*time.Hour))
	lock, err := os.OpenFile(filepath.Join(worktree, worktreeLock), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	if err := cleanWorktrees(root, now, worktreeMaxBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("held worktree removed: %v", err)
	}
}

func TestCleanWorktreesRemovesOldestAboveLimit(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	old := makeWorktree(t, root, "old", now.Add(-48*time.Hour))
	recent := makeWorktree(t, root, "recent", now.Add(-time.Hour))
	if err := os.WriteFile(filepath.Join(old, "allocation"), make([]byte, 2*1024*1024), 0o600); err != nil {
		t.Fatal(err)
	}
	limit, err := allocatedSize(recent)
	if err != nil {
		t.Fatal(err)
	}
	if err := cleanWorktrees(root, now, limit); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("oldest worktree remains, stat error = %v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("recent worktree missing: %v", err)
	}
}

func TestCleanWorktreesUsesMarkerMtimeAfterLock(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	worktree := makeWorktree(t, root, "touched", now.Add(-15*24*time.Hour))
	marker := filepath.Join(worktree, worktreeLock)
	if err := os.Chtimes(marker, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := cleanWorktrees(root, now, worktreeMaxBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("recently used worktree removed: %v", err)
	}
}

func TestFindWorktreesFindsContractDepth(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	root := filepath.Join(t.TempDir(), "work")
	worktree := filepath.Join(root, "example", "repo", "gate", "ws-1")
	if err := os.MkdirAll(worktree, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, worktreeLock), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(worktree, worktreeLock), now.Add(-15*24*time.Hour), now.Add(-15*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	found, err := findWorktrees(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].path != worktree {
		t.Fatalf("findWorktrees() = %+v; want the <owner>/<repo>/<lane>/<workspace> worktree", found)
	}
	if err := cleanWorktrees(root, now, worktreeMaxBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Fatalf("expired worktree at contract depth remains, stat error = %v", err)
	}
}

func TestWeeklyCachesPurgeWhenLastPurgeIsSevenDaysOld(t *testing.T) {
	for _, test := range []struct {
		name      string
		now       time.Time
		lastPurge time.Time
		wantEmpty bool
	}{
		{name: "missed Sunday purges Monday", now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC), lastPurge: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC), wantEmpty: true},
		{name: "fresh purge is kept", now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC), lastPurge: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := t.TempDir()
			for _, name := range []string{"go-mod", "bun"} {
				path := filepath.Join(state, "cache", name)
				if err := os.MkdirAll(path, 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "entry"), []byte("cache"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			stamp := filepath.Join(state, "cache", "last-weekly-purge")
			if err := os.WriteFile(stamp, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(stamp, test.lastPurge, test.lastPurge); err != nil {
				t.Fatal(err)
			}
			if err := gc(state, test.now); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"go-mod", "bun"} {
				entries, err := os.ReadDir(filepath.Join(state, "cache", name))
				if err != nil {
					t.Fatal(err)
				}
				if (len(entries) == 0) != test.wantEmpty {
					t.Fatalf("%s cache has %d entries; want empty=%t", name, len(entries), test.wantEmpty)
				}
			}
		})
	}
}

func makeWorktree(t *testing.T, root, name string, used time.Time) string {
	t.Helper()
	path := filepath.Join(root, "owner", "repo", "gate", name)
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(path, worktreeLock)
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(marker, used, used); err != nil {
		t.Fatal(err)
	}
	return path
}
