// Package main removes expired runner worktrees and bounds the host caches.
package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

const (
	worktreeLock        = ".rjc-lock"
	worktreeMaxAge      = 14 * 24 * time.Hour
	worktreeMaxBytes    = 150 * 1024 * 1024 * 1024
	goBuildMaxBytes     = 40 * 1024 * 1024 * 1024
	weeklyPurgeInterval = 7 * 24 * time.Hour
)

type worktree struct {
	path string
	used time.Time
	size int64
}

func newRootCommand(stdout, stderr io.Writer) (*cobra.Command, *int, error) {
	var state string
	code := new(int)
	command := &cobra.Command{
		Use:           "rjc-gates-gc",
		Short:         "Remove expired runner worktrees and bound host caches",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if state == "" {
				return errors.New("--state must not be empty")
			}
			if err := gc(state, time.Now()); err != nil {
				*code = 1
				return fmt.Errorf("rjc-gates-gc: %w", err)
			}
			return nil
		},
	}
	command.Flags().StringVar(&state, "state", "", "Runner state filesystem root (required).")
	if err := command.MarkFlagRequired("state"); err != nil {
		*code = 1
		return nil, code, fmt.Errorf("mark --state required: %w", err)
	}
	command.SetOut(stdout)
	command.SetErr(stderr)
	command.CompletionOptions.DisableDefaultCmd = true
	return command, code, nil
}

func run(args []string, stdout, stderr io.Writer) int {
	command, code, err := newRootCommand(stdout, stderr)
	if err == nil {
		command.SetArgs(args)
		err = command.Execute()
	}
	if err != nil {
		if _, writeErr := fmt.Fprintln(stderr, err); writeErr != nil {
			return 1
		}
		if *code == 0 {
			return 2
		}
	}
	return *code
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func gc(state string, now time.Time) error {
	if err := cleanWorktrees(filepath.Join(state, "work"), now, worktreeMaxBytes); err != nil {
		return fmt.Errorf("clean worktrees: %w", err)
	}
	if err := cleanGoBuildCache(state); err != nil {
		return fmt.Errorf("clean Go build cache: %w", err)
	}
	if err := purgeWeeklyCaches(state, now); err != nil {
		return err
	}
	return nil
}

// purgeWeeklyCaches empties go-mod and bun once the last purge is 7 days old,
// so a missed run catches up. A future-dated stamp counts as stale. It defers
// while any gate holds a worktree lock, and leaves the stamp unwritten so the
// next daily run retries.
func purgeWeeklyCaches(state string, now time.Time) error {
	stamp := filepath.Join(state, "cache", "last-weekly-purge")
	info, err := os.Stat(stamp)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err == nil {
		if delta := now.Sub(info.ModTime()); delta >= 0 && delta < weeklyPurgeInterval {
			return nil
		}
	}
	held, err := anyWorktreeHeld(filepath.Join(state, "work"))
	if err != nil {
		return err
	}
	if held {
		return nil
	}
	for _, name := range []string{"go-mod", "bun"} {
		if err := clearDirectory(filepath.Join(state, "cache", name)); err != nil {
			return fmt.Errorf("clean %s cache: %w", name, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(stamp), 0o750); err != nil {
		return err
	}
	return os.WriteFile(stamp, nil, 0o600)
}

// anyWorktreeHeld reports whether any contract-depth worktree lock is held.
func anyWorktreeHeld(root string) (bool, error) {
	worktrees, err := findWorktrees(root)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, tree := range worktrees {
		held, err := probeWorktreeLock(tree.path)
		if err != nil {
			return false, err
		}
		if held {
			return true, nil
		}
	}
	return false, nil
}

// probeWorktreeLock reports whether a run holds the worktree's exclusive lock.
// A shared, non-blocking lock fails only against an exclusive holder. A run that
// tries to lock during the probe's brief hold gets EWOULDBLOCK and a 409.
// A marker that vanished after discovery is not held.
func probeWorktreeLock(path string) (bool, error) {
	file, err := os.OpenFile(filepath.Join(path, worktreeLock), os.O_RDWR, 0) // #nosec G304 -- marker is under the discovered worktree.
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if err == nil {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	}
	closeErr := file.Close()
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, closeErr
}

func cleanGoBuildCache(state string) error {
	return cleanGoBuildCacheAtLimit(state, goBuildMaxBytes)
}

func cleanGoBuildCacheAtLimit(state string, maxBytes int64) error {
	root := filepath.Join(state, "cache", "go-build")
	size, err := allocatedSize(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if size <= maxBytes {
		return nil
	}
	held, err := anyWorktreeHeld(filepath.Join(state, "work"))
	if err != nil {
		return err
	}
	if held {
		return nil
	}
	return clearDirectory(root)
}

func clearDirectory(root string) error {
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func cleanWorktrees(root string, now time.Time, maxBytes int64) error {
	worktrees, err := findWorktrees(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	cutoff := now.Add(-worktreeMaxAge)
	for i := range worktrees {
		if worktrees[i].used.Before(cutoff) {
			removed, err := removeIfUnlockedBefore(worktrees[i].path, cutoff)
			if err != nil {
				return err
			}
			if removed {
				worktrees[i].size = 0
			}
		}
	}
	sort.Slice(worktrees, func(i, j int) bool { return worktrees[i].used.Before(worktrees[j].used) })
	var total int64
	for _, tree := range worktrees {
		total += tree.size
	}
	for _, tree := range worktrees {
		if total <= maxBytes {
			break
		}
		if tree.size == 0 {
			continue
		}
		removed, err := removeIfUnlocked(tree.path)
		if err != nil {
			return err
		}
		if removed {
			total -= tree.size
		}
	}
	return nil
}

// contractDepth is the path depth of a worktree below the work root:
// <owner>/<repo>/<lane>/<workspace>, as rjc runner exec creates it.
const contractDepth = 4

// findWorktrees returns the worktrees at contract depth. It never descends
// past that depth, so nested checkouts such as node_modules are not walked.
func findWorktrees(root string) ([]worktree, error) {
	var result []worktree
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if !entry.IsDir() || path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		depth := strings.Count(rel, string(filepath.Separator)) + 1
		if depth > contractDepth {
			return filepath.SkipDir
		}
		if depth < contractDepth {
			return nil
		}
		info, err := os.Stat(filepath.Join(path, worktreeLock))
		if errors.Is(err, fs.ErrNotExist) {
			return filepath.SkipDir
		}
		if err != nil {
			return err
		}
		size, err := allocatedSize(path)
		if err != nil {
			return err
		}
		result = append(result, worktree{path: path, used: info.ModTime(), size: size})
		return filepath.SkipDir
	})
	return result, err
}

func removeIfUnlocked(path string) (bool, error) {
	return removeIfUnlockedBefore(path, time.Time{})
}

func removeIfUnlockedBefore(path string, cutoff time.Time) (removed bool, resultErr error) {
	marker := filepath.Join(path, worktreeLock)
	file, err := os.OpenFile(marker, os.O_RDWR, 0) // #nosec G304 -- marker is under the discovered worktree.
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() {
		if err := file.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close worktree lock: %w", err))
		}
	}()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return false, nil
		}
		return false, err
	}
	defer func() {
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("unlock worktree lock: %w", err))
		}
	}()
	if !cutoff.IsZero() {
		info, err := file.Stat()
		if err != nil {
			return false, err
		}
		if !info.ModTime().Before(cutoff) {
			return false, nil
		}
	}
	if err := os.RemoveAll(path); err != nil {
		return false, err
	}
	return true, nil
}

func allocatedSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			total += stat.Blocks * 512
		}
		return nil
	})
	return total, err
}
