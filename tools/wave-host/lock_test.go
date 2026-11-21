package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestFileLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	lock, err := lockFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	other, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := other.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := unix.Flock(int(other.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
		t.Fatal("second lock acquired")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(other.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(other.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
}

func TestFileLockWaitsForContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	hold, err := lockFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := hold.Close(); err != nil {
			t.Error(err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	got, err := lockFile(ctx, path)
	if got != nil {
		if closeErr := got.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal("contended lock acquired")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock error = %v, want context deadline exceeded", err)
	}
}
