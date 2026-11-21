package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestCleanGoBuildCacheAtLimitMissingRootIsNil(t *testing.T) {
	if err := cleanGoBuildCacheAtLimit(t.TempDir(), 1); err != nil {
		t.Fatalf("missing root returned %v; want nil", err)
	}
}

func TestWeeklyPurgeSkippedWhileWorktreeLockHeld(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
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
	worktree := makeWorktree(t, filepath.Join(state, "work"), "busy", now.Add(-time.Hour))
	lock, err := os.OpenFile(filepath.Join(worktree, worktreeLock), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	if err := gc(state, now); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"go-mod", "bun"} {
		entries, err := os.ReadDir(filepath.Join(state, "cache", name))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == 0 {
			t.Fatalf("%s cache emptied while a gate holds a worktree lock", name)
		}
	}
	if _, err := os.Stat(filepath.Join(state, "cache", "last-weekly-purge")); !os.IsNotExist(err) {
		t.Fatalf("purge stamp written while a worktree lock is held, stat error = %v", err)
	}
}

func TestAnyWorktreeHeldTreatsMarkerRemovedMidProbeAsNotHeld(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	root := filepath.Join(t.TempDir(), "work")
	worktree := filepath.Join(root, "example", "repo", "gate", "ws-1")
	if err := os.MkdirAll(worktree, 0o750); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(worktree, worktreeLock)
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(marker, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Remove the marker after discovery and before the probe opens it, the
	// window a worktree can be deleted in. The probe must not fail the run.
	found, err := findWorktrees(root)
	if err != nil || len(found) != 1 {
		t.Fatalf("findWorktrees() = %+v, %v; want one worktree", found, err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	held, err := probeWorktreeLock(found[0].path)
	if err != nil {
		t.Fatalf("probeWorktreeLock() error = %v; a vanished marker must not fail the run", err)
	}
	if held {
		t.Fatal("probeWorktreeLock() = held; a vanished marker is not held")
	}
}

func TestProbeWorktreeLockReportsExclusiveHolder(t *testing.T) {
	root := filepath.Join(t.TempDir(), "work")
	worktree := filepath.Join(root, "example", "repo", "gate", "held")
	if err := os.MkdirAll(worktree, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, worktreeLock), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(worktree, worktreeLock), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	held, err := probeWorktreeLock(worktree)
	if err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Fatal("probeWorktreeLock() = not held; an exclusive holder must block the probe")
	}
}

func TestFindWorktreesIgnoresMarkerBelowContractDepth(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	root := filepath.Join(t.TempDir(), "work")
	nested := filepath.Join(root, "example", "repo", "gate", "ws-1", "node_modules", "pkg")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, worktreeLock), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(nested, worktreeLock), now.Add(-15*24*time.Hour), now.Add(-15*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	found, err := findWorktrees(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("findWorktrees() = %+v; a marker below contract depth must not be a worktree", found)
	}
}

func TestWeeklyPurgeTreatsFutureStampAsStale(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	state := t.TempDir()
	path := filepath.Join(state, "cache", "go-mod")
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "entry"), []byte("cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	stamp := filepath.Join(state, "cache", "last-weekly-purge")
	if err := os.WriteFile(stamp, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	future := now.Add(48 * time.Hour)
	if err := os.Chtimes(stamp, future, future); err != nil {
		t.Fatal(err)
	}
	if err := purgeWeeklyCaches(state, now); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("go-mod has %d entries; a future-dated stamp must be treated as stale", len(entries))
	}
}
