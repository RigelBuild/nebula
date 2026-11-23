package fence

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	breakerSuffix = ".break"
	writeGrace    = 5 * time.Minute
)

var (
	errLiveLock           = errors.New("live gc lock")
	errTakeoverInProgress = errors.New("gc lock takeover in progress")
)

type heldLock struct {
	path string
	file *os.File
}

type lockSnapshot struct {
	info os.FileInfo
	text string
}

// Lock takes dir's gc.lock (breaking a dead holder) and returns its release; it waits until deadline.
func Lock(ctx context.Context, dir string, deadline time.Time) (func() error, error) {
	lock, err := acquire(ctx, dir, deadline)
	if err != nil {
		return nil, err
	}
	return lock.release, nil
}

func acquire(ctx context.Context, agentDir string, deadline time.Time) (*heldLock, error) {
	if err := os.MkdirAll(agentDir, 0o750); err != nil {
		return nil, fmt.Errorf("create agent directory: %w", err)
	}
	lockPath := filepath.Join(agentDir, "gc.lock")
	for {
		if err := ctx.Err(); err != nil {
			return nil, notAcquired(agentDir, err)
		}
		file, err := openExclusive(lockPath)
		if err != nil {
			return nil, err
		}
		if file != nil {
			if err := writeLock(file); err != nil {
				return nil, errors.Join(fmt.Errorf("write gc lock: %w", err), releaseFile(lockPath, file))
			}
			return &heldLock{path: lockPath, file: file}, nil
		}

		file, err = breakDeadLock(lockPath)
		if err == nil && file != nil {
			if err := writeLock(file); err != nil {
				return nil, errors.Join(fmt.Errorf("write gc lock: %w", err), releaseFile(lockPath, file))
			}
			return &heldLock{path: lockPath, file: file}, nil
		}
		if err != nil && file != nil {
			return nil, errors.Join(err, releaseFile(lockPath, file))
		}
		if err != nil && !errors.Is(err, errLiveLock) && !errors.Is(err, errTakeoverInProgress) {
			return nil, err
		}

		if !time.Now().Before(deadline) {
			return nil, &ExitError{Code: 4, Message: "a live process still holds " + lockPath}
		}
		timer := time.NewTimer(lockPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, notAcquired(agentDir, ctx.Err())
		case <-timer.C:
		}
	}
}

// notAcquired reports that ctx ended before agentDir's gc lock was taken. Run
// returns it, so the CLI exits non-zero. A cancel after every lock is held is a
// normal stop and returns nil instead.
func notAcquired(agentDir string, err error) error {
	return fmt.Errorf("gc lock in %s not acquired: %w", agentDir, err)
}

func openExclusive(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // The path is gc.lock under the operator-selected agent directory.
	if err == nil {
		return file, nil
	}
	if errors.Is(err, os.ErrExist) {
		return nil, nil //nolint:nilnil // A lock already exists; the caller checks whether its PID is live.
	}
	return nil, fmt.Errorf("open gc lock: %w", err)
}

func writeLock(file *os.File) error {
	stamp := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	contents := fmt.Sprintf("%d\n%s\n", os.Getpid(), stamp)
	written, err := io.WriteString(file, contents)
	if err != nil {
		return err
	}
	if written != len(contents) {
		return io.ErrShortWrite
	}
	return nil
}

func breakDeadLock(lockPath string) (result *os.File, runErr error) {
	breakerPath := lockPath + breakerSuffix
	breaker, err := openBreaker(breakerPath)
	if err != nil {
		return nil, err
	}
	if err := writeLock(breaker); err != nil {
		return nil, errors.Join(fmt.Errorf("write gc breaker: %w", err), releaseFile(breakerPath, breaker))
	}
	defer func() {
		if releaseErr := releaseFile(breakerPath, breaker); releaseErr != nil {
			runErr = errors.Join(runErr, releaseErr)
		}
	}()

	file, err := openExclusive(lockPath)
	if err != nil {
		return nil, err
	}
	if file != nil {
		return file, nil
	}
	snapshot, err := readLockSnapshot(lockPath)
	if err != nil {
		return nil, err
	}
	if snapshot == nil || !shouldBreakLock(*snapshot) || !snapshotStillCurrent(lockPath, *snapshot) {
		return nil, errLiveLock
	}
	if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale gc lock: %w", err)
	}
	takeover, err := openExclusive(lockPath)
	if err != nil {
		return nil, err
	}
	if takeover == nil {
		return nil, errLiveLock
	}
	return takeover, nil
}

// openBreaker takes the gc.lock.break mutex that serializes takeovers. A stale
// breaker from a crashed takeover is removed and the open is tried once more,
// so a dead gc.lock is not held up for another poll interval. A breaker still
// held after that belongs to a live takeover.
func openBreaker(breakerPath string) (*os.File, error) {
	breaker, err := openExclusive(breakerPath)
	if err != nil {
		return nil, err
	}
	if breaker != nil {
		return breaker, nil
	}
	snapshot, err := readLockSnapshot(breakerPath)
	if err != nil {
		return nil, err
	}
	if snapshot == nil || !shouldBreakLock(*snapshot) || !snapshotStillCurrent(breakerPath, *snapshot) {
		return nil, errTakeoverInProgress
	}
	if err := os.Remove(breakerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale gc breaker: %w", err)
	}
	breaker, err = openExclusive(breakerPath)
	if err != nil {
		return nil, err
	}
	if breaker == nil {
		return nil, errTakeoverInProgress
	}
	return breaker, nil
}

func readLockSnapshot(path string) (*lockSnapshot, error) {
	before, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil //nolint:nilnil // A missing file can be removed by a concurrent lock owner.
		}
		return nil, fmt.Errorf("stat gc lock: %w", err)
	}
	contents, err := os.ReadFile(path) //nolint:gosec // The path is the gc.lock entry under the operator-selected agent directory.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil //nolint:nilnil // A missing file can be removed by a concurrent lock owner.
		}
		return nil, fmt.Errorf("read gc lock: %w", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil //nolint:nilnil // A missing file can be removed by a concurrent lock owner.
		}
		return nil, fmt.Errorf("restat gc lock: %w", err)
	}
	if !sameLockFile(before, after) {
		return nil, nil //nolint:nilnil // The lock changed during the snapshot; retry before deciding to break it.
	}
	return &lockSnapshot{info: after, text: string(contents)}, nil
}

func sameLockFile(left, right os.FileInfo) bool {
	if !os.SameFile(left, right) || left.Size() != right.Size() || !left.ModTime().Equal(right.ModTime()) {
		return false
	}
	return changeTime(left) == changeTime(right)
}

func snapshotStillCurrent(path string, snapshot lockSnapshot) bool {
	info, err := os.Stat(path)
	return err == nil && info != nil && sameLockFile(snapshot.info, info)
}

func shouldBreakLock(snapshot lockSnapshot) bool {
	lines := strings.SplitN(snapshot.text, "\n", 3)
	if pid, ok := lockPID(lines[0]); ok {
		return !processExists(pid)
	}
	createdAt := snapshot.info.ModTime()
	if len(lines) >= 2 {
		if parsedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(lines[1])); err == nil {
			createdAt = parsedAt
		}
	}
	return time.Since(createdAt) > writeGrace
}

// lockPID reads the numeric prefix of a lock's PID line. A PID above
// math.MaxInt32 is returned as is, so processExists treats the lock as live and
// never breaks it. Digits past the range of int64 clamp to MaxInt64 and are
// returned the same way.
func lockPID(line string) (int64, bool) {
	text := strings.TrimLeft(line, " \t")
	digits := strings.TrimLeft(text, "+-")
	end := 0
	for end < len(digits) && digits[end] >= '0' && digits[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	pid, err := strconv.ParseInt(text[:len(text)-len(digits)+end], 10, 64)
	if (err != nil && !errors.Is(err, strconv.ErrRange)) || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// processExists reports whether pid names a live process. A PID above
// math.MaxInt32 counts as live without a probe, so syscall.Kill never sees it.
func processExists(pid int64) bool {
	if pid > math.MaxInt32 {
		return true
	}
	err := syscall.Kill(int(pid), 0)
	return err == nil || (!errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.EINVAL))
}

func releaseFile(path string, file *os.File) error {
	closeErr := file.Close()
	removeErr := os.Remove(path)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	return errors.Join(closeErr, removeErr)
}

func (lock *heldLock) release() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	return releaseFile(lock.path, lock.file)
}
