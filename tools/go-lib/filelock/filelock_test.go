package filelock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestWithRunsWorkUnderFileLock(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "session.lock")
	called := false
	if err := With(t.Context(), lockPath, func() error {
		called = true
		info, err := os.Stat(lockPath)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return errors.New("lock path is a directory")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("work was not called")
	}
}

func TestWithDoesNotDependOnPATH(t *testing.T) {
	t.Setenv("PATH", "")
	called := false
	err := With(t.Context(), filepath.Join(t.TempDir(), "session.lock"), func() error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("work was not called")
	}
}

func TestWithReturnsWorkError(t *testing.T) {
	want := errors.New("work failed")
	err := With(t.Context(), filepath.Join(t.TempDir(), "session.lock"), func() error {
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("With() error = %v, want wrapped %v", err, want)
	}
}

func TestWithCreatesPrivateLockFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.lock")
	if err := With(t.Context(), path, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("lock mode = %04o, want 0600", got)
	}
}

func TestWithRefusesSymlinkedLockFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	const content = "do not touch"
	if err := os.WriteFile(target, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dir, "session.lock")
	if err := os.Symlink(target, lockPath); err != nil {
		t.Fatal(err)
	}
	ran := false
	err = With(t.Context(), lockPath, func() error {
		ran = true
		return nil
	})
	if !errors.Is(err, unix.ELOOP) {
		t.Fatalf("With() error = %v, want ELOOP for symlinked lock", err)
	}
	if ran {
		t.Fatal("With() ran work through a symlinked lock")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Fatalf("symlink target content = %q, want %q", got, content)
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode().Perm() != before.Mode().Perm() {
		t.Fatalf("symlink target mode = %04o, want %04o", after.Mode().Perm(), before.Mode().Perm())
	}
}

func TestWithHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := With(ctx, filepath.Join(t.TempDir(), "lock"), func() error {
		t.Fatal("work ran with canceled context")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("With() error = %v, want cancellation", err)
	}
}

func TestWithHonorsCancellationWhileWaitingForLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.lock")
	holder, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX); err != nil {
		_ = holder.Close()
		t.Fatal(err)
	}
	defer func() {
		if err := unix.Flock(int(holder.Fd()), unix.LOCK_UN); err != nil {
			t.Error(err)
		}
		if err := holder.Close(); err != nil {
			t.Error(err)
		}
	}()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	contended := make(chan struct{})
	workRan := false
	done := make(chan error, 1)
	go func() {
		done <- with(ctx, path, func() error {
			workRan = true
			return nil
		}, func() { close(contended) }, lockTimeout)
	}()
	select {
	case <-contended:
	case <-time.After(time.Second):
		t.Fatal("With() did not report lock contention")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("With() while waiting error = %v, want context canceled", err)
	}
	if workRan {
		t.Fatal("With() ran work after cancellation")
	}
}

func TestWithCancellationAfterAcquisitionKeepsLockUntilWorkReturns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.lock")
	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan struct{})
	finish := make(chan struct{})
	var finishOnce sync.Once
	releaseWork := func() { finishOnce.Do(func() { close(finish) }) }
	defer releaseWork()
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- With(ctx, path, func() error {
			close(started)
			<-finish
			return nil
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first work did not acquire lock")
	}
	cancel()
	blocked := make(chan struct{})
	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- with(t.Context(), path, func() error {
			close(secondEntered)
			return nil
		}, func() { close(blocked) }, lockTimeout)
	}()
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("second acquisition did not report lock contention")
	}
	select {
	case <-secondEntered:
		t.Fatal("second work entered before the first work returned")
	default:
	}
	probe, err := os.OpenFile(path, os.O_RDONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	err = unix.Flock(int(probe.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	closeErr := probe.Close()
	if !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("fresh fd flock error = %v, want EWOULDBLOCK while first work holds lock", err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	releaseWork()
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-secondEntered:
	case <-time.After(time.Second):
		t.Fatal("second work did not enter after the first released the lock")
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
}

func TestWithReleasesLockWhenWorkReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.lock")
	want := errors.New("work failed")
	if err := With(t.Context(), path, func() error { return want }); !errors.Is(err, want) {
		t.Fatalf("With() error = %v, want %v", err, want)
	}
	if err := With(t.Context(), path, func() error { return nil }); err != nil {
		t.Fatalf("lock was not released after work error: %v", err)
	}
}

func TestWithReleasesLockWhenWorkExitsGoroutine(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "session.lock")
	returned := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		err := With(ctx, path, func() error {
			runtime.Goexit()
			return nil
		})
		returned <- err
	}()
	<-exited
	select {
	case err := <-returned:
		t.Fatalf("With() returned after work exited goroutine: %v", err)
	default:
	}
	if err := With(ctx, path, func() error { return nil }); err != nil {
		t.Fatalf("lock was not released after work exited goroutine: %v", err)
	}
}

func TestWithTimesOutWhileLockHeldByAnotherFD(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.lock")
	holder, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := holder.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	workRan := false
	err = with(t.Context(), path, func() error {
		workRan = true
		return nil
	}, nil, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out after 50ms") {
		t.Fatalf("with() error = %v, want timed out after 50ms", err)
	}
	if workRan {
		t.Fatal("with() ran work while another fd held the lock")
	}
}

func TestTryLockReturnsErrorForClosedFile(t *testing.T) {
	file, err := os.OpenFile(filepath.Join(t.TempDir(), "session.lock"), os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	contended, err := tryLock(t.Context(), file, "session.lock")
	if contended || !errors.Is(err, unix.EBADF) {
		t.Fatalf("tryLock() on closed file = contended %t, err %v; want EBADF", contended, err)
	}
}
