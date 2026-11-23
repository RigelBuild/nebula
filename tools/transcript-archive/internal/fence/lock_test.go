package fence

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const deadPID = "2147483647"

// ageFile sets path's mtime to age in the past, so writeGrace checks see an old file.
func ageFile(t *testing.T, path string, age time.Duration) {
	t.Helper()
	old := time.Now().Add(-age)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("age %s: %v", path, err)
	}
}

func releaseTestLock(t *testing.T, lock *heldLock) {
	t.Helper()
	if err := lock.release(); err != nil {
		t.Errorf("release gc lock: %v", err)
	}
}

// requireKeptLock runs acquire with a deadline that has already passed. A lock
// that is kept then surfaces as exit code 4 at once, without any wait.
func requireKeptLock(t *testing.T, agentDir string) {
	t.Helper()
	started := time.Now()
	lock, err := acquire(t.Context(), agentDir, started)
	if time.Since(started) >= lockPollInterval {
		t.Fatal("acquire waited a poll interval after its deadline had passed")
	}
	if err == nil {
		releaseTestLock(t, lock)
		t.Fatal("acquire took a lock that should have been kept")
	}
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 4 {
		t.Fatalf("acquire error = %v, want exit code 4 for a kept lock", err)
	}
}

func TestAcquireTakesLockFromDeadPID(t *testing.T) {
	agentDir := t.TempDir()
	lockPath := filepath.Join(agentDir, "gc.lock")
	writeTestFile(t, lockPath, []byte(deadPID+"\n2026-01-01T00:00:00.000Z\n"))

	lock, err := acquire(t.Context(), agentDir, time.Now())
	if err != nil {
		t.Fatalf("acquire over dead-PID lock: %v", err)
	}
	t.Cleanup(func() { releaseTestLock(t, lock) })

	contents, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read taken lock: %v", err)
	}
	if want := strconv.Itoa(os.Getpid()) + "\n"; !strings.HasPrefix(string(contents), want) {
		t.Fatalf("lock = %q, want own PID first %q", contents, want)
	}
}

func TestAcquireBreaksStaleBreakerBeforeTakingDeadLock(t *testing.T) {
	agentDir := t.TempDir()
	lockPath := filepath.Join(agentDir, "gc.lock")
	breakerPath := lockPath + breakerSuffix
	writeTestFile(t, lockPath, []byte(deadPID+"\n2026-01-01T00:00:00.000Z\n"))
	writeTestFile(t, breakerPath, []byte(deadPID+"\n2026-01-01T00:00:00.000Z\n"))

	// The deadline has already passed. Without the stale-breaker retry, acquire
	// would return errTakeoverInProgress and exit 4 here instead of taking the lock.
	lock, err := acquire(t.Context(), agentDir, time.Now())
	if err != nil {
		t.Fatalf("acquire with stale breaker and dead lock: %v", err)
	}
	t.Cleanup(func() { releaseTestLock(t, lock) })
	if _, err := os.Stat(breakerPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("acquire left the stale gc breaker behind")
	}
}

func TestAcquireBreaksNoPIDLockOlderThanWriteGrace(t *testing.T) {
	agentDir := t.TempDir()
	lockPath := filepath.Join(agentDir, "gc.lock")
	writeTestFile(t, lockPath, []byte("not a pid\n"))
	ageFile(t, lockPath, 2*writeGrace)

	lock, err := acquire(t.Context(), agentDir, time.Now())
	if err != nil {
		t.Fatalf("acquire over aged no-PID lock: %v", err)
	}
	t.Cleanup(func() { releaseTestLock(t, lock) })
}

func TestAcquireKeepsNoPIDLockYoungerThanWriteGrace(t *testing.T) {
	agentDir := t.TempDir()
	lockPath := filepath.Join(agentDir, "gc.lock")
	writeTestFile(t, lockPath, []byte("not a pid\n"))

	requireKeptLock(t, agentDir)
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatal("acquire removed a no-PID lock younger than writeGrace")
	}
}

func TestAcquireKeepsLockWithPIDAboveInt32(t *testing.T) {
	agentDir := t.TempDir()
	lockPath := filepath.Join(agentDir, "gc.lock")
	// 2^31 does not fit pid_t, but the lock still counts as live. The timestamp
	// is old on purpose: a timestamp-only judgement would break this lock, so
	// only the PID rule keeps it.
	stamp := time.Now().Add(-2 * writeGrace).UTC().Format(time.RFC3339Nano)
	writeTestFile(t, lockPath, []byte("2147483648\n"+stamp+"\n"))

	requireKeptLock(t, agentDir)
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatal("acquire broke a lock whose PID exceeds MaxInt32")
	}
}

func TestShouldBreakLockReadsOnlySecondLineTimestamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gc.lock")
	writeTestFile(t, path, nil)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal("stat lock fixture")
	}
	// The file mtime is fresh. Line 2 is older than writeGrace, and line 3 is an
	// extra line. Only line 2 may decide, so the lock must break.
	text := "not a pid\n2026-01-01T00:00:00.000Z\nextra\n"
	if !shouldBreakLock(lockSnapshot{info: info, text: text}) {
		t.Fatal("shouldBreakLock ignored the stale timestamp on line 2")
	}
}

func TestSameLockFileDetectsChangeTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gc.lock")
	writeTestFile(t, path, []byte("1\n"))
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal("stat lock fixture")
	}
	if changeTime(before) == 0 {
		t.Skip("change time is not readable on this platform")
	}

	// Rewrite with the same size and restore the mtime, so size and mtime match
	// and only the change time differs. Chtimes also moves ctime, and the kernel
	// clock is coarse, so retry at a fixed pace until ctime has moved past before's.
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var after os.FileInfo
	for {
		if err := os.WriteFile(path, []byte("2\n"), 0o600); err != nil {
			t.Fatal("rewrite lock fixture")
		}
		if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
			t.Fatal("restore lock mtime")
		}
		after, err = os.Stat(path)
		if err != nil {
			t.Fatal("restat lock fixture")
		}
		if changeTime(after) != changeTime(before) {
			break
		}
		select {
		case <-timer.C:
			t.Skip("filesystem change time did not advance; ctime comparison is not observable here")
		case <-ticker.C:
		}
	}
	if changeTime(after) == changeTime(before) {
		t.Skip("filesystem change time did not advance; ctime comparison is not observable here")
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("fixture did not keep size and mtime, so the ctime check is not isolated")
	}
	if sameLockFile(before, after) {
		t.Fatal("sameLockFile matched a lock whose change time advanced")
	}
}
