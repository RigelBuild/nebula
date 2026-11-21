// SPDX-License-Identifier: MIT OR Apache-2.0

// Package filelock runs work while holding an exclusive file lock.
package filelock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

const (
	lockTimeout       = 60 * time.Second
	lockRetryInterval = 10 * time.Millisecond
)

// With runs fn while holding an exclusive lock on path.
func With(ctx context.Context, path string, fn func() error) error {
	return with(ctx, path, fn, nil, lockTimeout)
}

func with(ctx context.Context, path string, fn func() error, onContended func(), timeout time.Duration) (returnErr error) {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("acquiring lock %s: %w", path, err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY|unix.O_NOFOLLOW, 0o600) // #nosec G304 -- caller supplies the lock path.
	if err != nil {
		return fmt.Errorf("opening lock %s: %w", path, err)
	}
	locked := false
	defer func() {
		if locked {
			if err := unix.Flock(int(file.Fd()), unix.LOCK_UN); err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("releasing lock %s: %w", path, err))
			}
		}
		if err := file.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("closing lock %s: %w", path, err))
		}
	}()
	if err := acquire(ctx, file, path, onContended, timeout); err != nil {
		return err
	}
	locked = true
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("acquiring lock %s: %w", path, err)
	}
	return fn()
}

// acquire polls LOCK_NB so cancellation never depends on interrupting a blocking flock syscall.
func acquire(ctx context.Context, file *os.File, path string, onContended func(), timeout time.Duration) error {
	contended, err := tryLock(ctx, file, path)
	if err != nil || !contended {
		return err
	}
	if onContended != nil {
		onContended()
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	retry := time.NewTicker(lockRetryInterval)
	defer retry.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("acquiring lock %s: %w", path, ctx.Err())
		case <-timer.C:
			return fmt.Errorf("acquiring lock %s: timed out after %s", path, timeout)
		case <-retry.C:
		}
		if contended, err = tryLock(ctx, file, path); err != nil || !contended {
			return err
		}
	}
}

// tryLock reports contended=true when another holder has the lock.
func tryLock(ctx context.Context, file *os.File, path string) (contended bool, err error) {
	for {
		if err := ctx.Err(); err != nil {
			return false, fmt.Errorf("acquiring lock %s: %w", path, err)
		}
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		switch {
		case err == nil:
			return false, nil
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN):
			return true, nil
		default:
			return false, fmt.Errorf("acquiring lock %s: %w", path, err)
		}
	}
}
