package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

const lockRetryInterval = 10 * time.Millisecond

func lockFile(ctx context.Context, path string) (*os.File, error) {
	//nolint:gosec // The caller configures the lock file path.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	acquire := func() error {
		return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(fmt.Errorf("acquire lock: %w", err), file.Close())
	}
	if err := acquire(); err == nil {
		return file, nil
	} else if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
		return nil, errors.Join(fmt.Errorf("acquire lock: %w", err), file.Close())
	}

	ticker := time.NewTicker(lockRetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, errors.Join(fmt.Errorf("acquire lock: %w", ctx.Err()), file.Close())
		case <-ticker.C:
			if err := ctx.Err(); err != nil {
				return nil, errors.Join(fmt.Errorf("acquire lock: %w", err), file.Close())
			}
			if err := acquire(); err == nil {
				return file, nil
			} else if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
				return nil, errors.Join(fmt.Errorf("acquire lock: %w", err), file.Close())
			}
		}
	}
}
