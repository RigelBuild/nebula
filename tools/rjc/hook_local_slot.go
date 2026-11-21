package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func acquireLocalGateSlot(ctx context.Context, env []string, log io.Writer) (func() error, error) {
	slots := 1
	for _, name := range []string{"RJC_HOOK_SSH_SLOTS", "GATE_SLOTS"} {
		if value := envLookup(env, name); value != "" {
			var err error
			slots, err = strconv.Atoi(value)
			if err != nil || slots < 1 {
				return nil, fmt.Errorf("%s must be a positive integer, got %q", name, value)
			}
			break
		}
	}
	dir := envLookup(env, "XDG_RUNTIME_DIR")
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "rjc-gate-slots-"+strconv.Itoa(os.Getuid()))
	} else {
		dir = filepath.Join(dir, "rjc-gate-slots")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create local gate slot directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("inspect local gate slot directory: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm() != 0o700 || !ok || int64(stat.Uid) != int64(os.Getuid()) {
		return nil, fmt.Errorf("local gate slot directory must be owned by the current user and mode 0700: %s", dir)
	}
	var lastProgress time.Time
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("wait for local gate slot: %w", err)
		}
		for index := 1; index <= slots; index++ {
			file, err := os.OpenFile(filepath.Join(dir, "slot-"+strconv.Itoa(index)), os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // The directory is selected by the local user.
			if err != nil {
				return nil, fmt.Errorf("open local gate slot: %w", err)
			}
			err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
			if err == nil {
				return func() error { return errors.Join(unix.Flock(int(file.Fd()), unix.LOCK_UN), file.Close()) }, nil
			}
			closeErr := file.Close()
			if closeErr != nil || (!errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN)) {
				return nil, errors.Join(fmt.Errorf("acquire local gate slot: %w", err), closeErr)
			}
		}
		if time.Since(lastProgress) >= 30*time.Second {
			if _, err := fmt.Fprintln(log, "hooks: waiting for a local gate slot"); err != nil {
				return nil, fmt.Errorf("write local gate slot progress: %w", err)
			}
			lastProgress = time.Now()
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("wait for local gate slot: %w", ctx.Err())
		case <-timer.C:
		}
	}
}
