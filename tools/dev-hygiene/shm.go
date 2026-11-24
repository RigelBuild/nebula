package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type shmDeps struct {
	now   func() time.Time
	owned func(fs.FileInfo) bool
	held  func() (map[string]struct{}, error)
}

func defaultShmDeps() shmDeps {
	return shmDeps{
		now: time.Now,
		owned: func(info fs.FileInfo) bool {
			stat, ok := info.Sys().(*syscall.Stat_t)
			return ok && stat.Uid == uint32(os.Getuid())
		},
		held: shmHeldPaths,
	}
}

func pruneShm(ctx context.Context, root string, age time.Duration, apply bool, out *printer, deps shmDeps) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve shm root: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve shm root: %w", err)
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read shm root: %w", err)
	}
	held, err := shmCanonicalHeldPaths(deps.held)
	if err != nil {
		return fmt.Errorf("scan held paths: %w", err)
	}
	var count int
	var total int64
	var errs []error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(root, entry.Name())
		info, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("stat shm entry %s: %w", path, err)
		}
		if !deps.owned(info) || shmHeld(path, held) {
			continue
		}
		if info.IsDir() {
			owned, err := shmOwnedChildren(path, deps.owned)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return fmt.Errorf("inspect shm ownership %s: %w", path, err)
			}
			if !owned {
				continue
			}
		}
		old, size, err := shmAgeAndSize(path, deps.now().Add(-age))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect shm entry %s: %w", path, err)
		}
		if !old {
			continue
		}
		if apply {
			old, size, err = shmAgeAndSize(path, deps.now().Add(-age))
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return fmt.Errorf("recheck shm entry %s: %w", path, err)
			}
			if !old {
				continue
			}
			held, err = shmCanonicalHeldPaths(deps.held)
			if err != nil {
				return fmt.Errorf("recheck held paths: %w", err)
			}
			if shmHeld(path, held) {
				continue
			}
			info, err = os.Lstat(path)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return fmt.Errorf("recheck shm entry %s: %w", path, err)
			}
			if !deps.owned(info) {
				continue
			}
			if info.IsDir() {
				owned, err := shmOwnedChildren(path, deps.owned)
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				if err != nil {
					return fmt.Errorf("recheck shm ownership %s: %w", path, err)
				}
				if !owned {
					continue
				}
				err = os.RemoveAll(path)
			} else {
				err = os.Remove(path)
			}
			// One stuck entry must not stop the rest of the pass from reclaiming RAM.
			if err != nil {
				errs = append(errs, fmt.Errorf("remove shm entry %s: %w", path, err))
				continue
			}
		}
		count++
		total += size
	}
	out.printf("shm: %d stale item(s), %d byte(s)%s\n", count, total, applyNote(apply))
	return errors.Join(errs...)
}

func shmAgeAndSize(path string, cutoff time.Time) (bool, int64, error) {
	old := true
	var size int64
	err := filepath.WalkDir(path, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.ModTime().After(cutoff) {
			old = false
		}
		if info.Mode().IsRegular() {
			size += info.Size()
		}
		return nil
	})
	return old, size, err
}

func shmOwnedChildren(root string, owned func(fs.FileInfo) bool) (bool, error) {
	allOwned := true
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !owned(info) {
			allOwned = false
			return fs.SkipAll
		}
		return nil
	})
	return allOwned, err
}

func shmHeld(path string, held map[string]struct{}) bool {
	for candidate := range held {
		if within(candidate, path) {
			return true
		}
	}
	return false
}

func shmCanonicalHeldPaths(scan func() (map[string]struct{}, error)) (map[string]struct{}, error) {
	paths, err := scan()
	if err != nil {
		return nil, err
	}
	canonical := make(map[string]struct{}, len(paths)*4)
	for path := range paths {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve held path %q: %w", path, err)
		}
		absolute = filepath.Clean(absolute)
		canonical[path] = struct{}{}
		canonical[absolute] = struct{}{}

		resolved, err := filepath.EvalSymlinks(absolute)
		if err == nil {
			canonical[resolved] = struct{}{}
			continue
		}

		resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
		if err == nil {
			canonical[filepath.Join(resolvedParent, filepath.Base(absolute))] = struct{}{}
		}
	}
	return canonical, nil
}

func shmHeldPaths() (map[string]struct{}, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("read /proc: %w", err)
	}
	paths := make(map[string]struct{})
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		if err := shmProcessPaths(filepath.Join("/proc", entry.Name()), paths); err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
				continue
			}
			return nil, fmt.Errorf("scan pid %s: %w", entry.Name(), err)
		}
	}
	return paths, nil
}

func shmProcessPaths(proc string, paths map[string]struct{}) error {
	cwd, err := os.Readlink(filepath.Join(proc, "cwd"))
	if err != nil {
		return err
	}
	paths[cwd] = struct{}{}
	fds, err := os.ReadDir(filepath.Join(proc, "fd"))
	if err != nil {
		return err
	}
	for _, fd := range fds {
		target, err := os.Readlink(filepath.Join(proc, "fd", fd.Name()))
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
			continue
		}
		if err != nil {
			return err
		}
		paths[strings.TrimSuffix(target, " (deleted)")] = struct{}{}
	}
	maps, err := os.ReadFile(filepath.Join(proc, "maps"))
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(maps), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 6 {
			paths[strings.TrimSuffix(strings.Join(fields[5:], " "), " (deleted)")] = struct{}{}
		}
	}
	cmdline, err := os.ReadFile(filepath.Join(proc, "cmdline"))
	if err != nil {
		return err
	}
	for _, arg := range bytes.Split(cmdline, []byte{0}) {
		paths[string(arg)] = struct{}{}
	}
	return nil
}
