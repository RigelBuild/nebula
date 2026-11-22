package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// homeCache holds per-task dirs, which are pruned; homeCacheKept are shared build caches.
const homeCache = ".cache"

// homeContainers are job dirs. Their children are pruned; the containers stay.
// Under rjc-hook only tmp holds per-run dirs.
var homeContainers = []string{"harness", "build", "rjc-hook/tmp", "work", "workspaces", "tmp", "agents-ci"}

// homeCacheKept are the .cache entries that are shared build caches.
// golangci-lint and staticcheck are the Go lint caches shared across builds.
var homeCacheKept = []string{"go-build", "nix", "golangci-lint", "staticcheck"}

// homeKept are top-level names never removed. Dot-prefixed names are kept as well.
// rjc-hook holds the bare git caches, slot locks, the hook queue and the hk
// tool cache.
var homeKept = []string{"rbuild", "go", "hermes", "agents", "rjc-hook"}

// idleWalkMaxDepth bounds how deep the idle walk of one candidate descends.
const idleWalkMaxDepth = 16

// defaultWalkBudget bounds the idle walk of one candidate in wall time.
const defaultWalkBudget = 30 * time.Second

// errWalkBound stops an idle walk that passed its depth or time bound.
var errWalkBound = errors.New("idle walk bound reached")

// homeWalk overrides the idle walk's time budget and clock; tests inject both.
type homeWalk struct {
	budget time.Duration
	clock  func() time.Time
}

// homeScan selects home candidates and keeps trees whose walks exceed their bounds.
type homeScan struct {
	root    *os.Root
	now     time.Time
	maxIdle time.Duration
	budget  time.Duration
	clock   func() time.Time
	out     *printer
}

// selectCandidates returns the idle workspaces and staged leftovers to prune.
func selectCandidates(root *os.Root, now time.Time, opts options, out *printer) (candidates, leftovers []string, err error) {
	if opts.home {
		s := homeScan{root: root, now: now, maxIdle: opts.maxIdle, budget: opts.walk.budget, clock: opts.walk.clock, out: out}
		if s.budget <= 0 {
			s.budget = defaultWalkBudget
		}
		if s.clock == nil {
			s.clock = time.Now
		}
		if candidates, err = s.candidates(); err != nil {
			return nil, nil, err
		}
		leftovers, err = s.leftovers()
		return candidates, leftovers, err
	}
	if opts.pattern != "" {
		matches, err := fs.Glob(root.FS(), filepath.ToSlash(opts.pattern))
		if err != nil {
			return nil, nil, err
		}
		for _, rel := range matches {
			ok, err := safeGlobPath(root, rel, false)
			if err != nil {
				return nil, nil, err
			}
			if !ok {
				continue
			}
			idle, err := idleLongerThan(root, rel, now, opts.maxIdle)
			if err != nil {
				return nil, nil, err
			}
			if idle {
				candidates = append(candidates, rel)
			}
		}
		stagedPattern := filepath.Join(filepath.Dir(opts.pattern), pruneStagePrefix+filepath.Base(opts.pattern)+"-*")
		staged, err := fs.Glob(root.FS(), filepath.ToSlash(stagedPattern))
		if err != nil {
			return nil, nil, err
		}
		for _, rel := range staged {
			ok, err := safeGlobPath(root, rel, true)
			if err != nil {
				return nil, nil, err
			}
			if !ok {
				continue
			}
			idle, err := idleLongerThan(root, rel, now, opts.maxIdle)
			if err != nil {
				return nil, nil, err
			}
			if idle {
				leftovers = append(leftovers, rel)
			}
		}
		return candidates, leftovers, nil
	}
	if candidates, err = idleCandidates(root, now, opts.maxIdle); err != nil {
		return nil, nil, err
	}
	leftovers, err = stagedLeftovers(root, now, opts.maxIdle)
	return candidates, leftovers, err
}

// safeGlobPath refuses hidden components, bare caches and symlinked paths.
func safeGlobPath(root *os.Root, rel string, staged bool) (bool, error) {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	path := ""
	for i, part := range parts {
		if strings.HasSuffix(part, ".git") || (strings.HasPrefix(part, ".") && !(staged && i == len(parts)-1 && strings.HasPrefix(part, pruneStagePrefix))) {
			return false, nil
		}
		path = filepath.Join(path, part)
		info, err := root.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
			return false, nil
		}
	}
	return true, nil
}

// candidates returns the home entries idle longer than maxIdle: top-level entries
// that are not kept, plus the children of the containers and of .cache.
func (s homeScan) candidates() ([]string, error) {
	top, err := readDirFS(s.root, ".")
	if err != nil {
		return nil, fmt.Errorf("read home: %w", err)
	}
	var out []string
	for _, entry := range top {
		name := entry.Name()
		if protectedHome(name) || (entry.IsDir() && slices.Contains(homeContainers, name)) {
			continue
		}
		idle, err := s.treeIdle(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if idle {
			out = append(out, name)
		}
	}
	found, err := s.idleChildren(homeCache, homeCacheKept)
	if err != nil {
		return nil, err
	}
	out = append(out, found...)
	for _, dir := range homeContainers {
		found, err := s.idleChildren(dir, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	return out, nil
}

// idleChildren returns the root-relative entries directly under dir that are idle longer than
// maxIdle, skipping keep, dot-prefixed names and a dir that is gone or not a dir.
func (s homeScan) idleChildren(dir string, keep []string) ([]string, error) {
	entries, err := readDirFS(s.root, dir)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var out []string
	for _, entry := range entries {
		name := entry.Name()
		if protectedHome(name) || slices.Contains(keep, name) {
			continue
		}
		rel := filepath.Join(dir, name)
		idle, err := s.treeIdle(rel)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if idle {
			out = append(out, rel)
		}
	}
	return out, nil
}

// leftovers returns the staged .prune-* entries at the home top level and inside the
// containers and .cache that are idle longer than maxIdle.
func (s homeScan) leftovers() ([]string, error) {
	var out []string
	for _, dir := range append([]string{".", homeCache}, homeContainers...) {
		staged, err := s.stagedIn(dir)
		if err != nil {
			return nil, err
		}
		out = append(out, staged...)
	}
	return out, nil
}

// stagedIn returns the staged .prune-* entries directly under dir that are idle longer than maxIdle.
func (s homeScan) stagedIn(dir string) ([]string, error) {
	entries, err := readDirFS(s.root, dir)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var out []string
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), pruneStagePrefix) {
			continue
		}
		rel := filepath.Join(dir, entry.Name())
		idle, err := s.treeIdle(rel)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if idle {
			out = append(out, rel)
		}
	}
	return out, nil
}

// treeIdle checks all descendants without following symlinks. A tree whose walk
// exceeds the depth or time bound is kept.
func (s homeScan) treeIdle(rel string) (bool, error) {
	info, err := s.root.Lstat(rel)
	if err != nil {
		return false, err
	}
	if !s.idleAt(info.ModTime()) {
		return false, nil
	}
	if !info.IsDir() {
		return true, nil
	}
	baseDepth := strings.Count(filepath.ToSlash(rel), "/")
	deadline := s.clock().Add(s.budget)
	fresh := false
	err = fs.WalkDir(s.root.FS(), filepath.ToSlash(rel), func(path string, entry fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("walk %s: %w", path, err)
		}
		if strings.Count(path, "/")-baseDepth > idleWalkMaxDepth || s.clock().After(deadline) {
			return errWalkBound
		}
		info, err := entry.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}
		if !s.idleAt(info.ModTime()) {
			fresh = true
			return fs.SkipAll
		}
		return nil
	})
	if errors.Is(err, errWalkBound) {
		s.out.printf("walk bound reached in %s; keeping tree\n", rel)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !fresh, nil
}

// idleAt reports whether mtime is older than maxIdle.
func (s homeScan) idleAt(mtime time.Time) bool {
	return s.now.Sub(mtime) > s.maxIdle
}

// protectedHome reports whether a home entry is never a candidate: dot-prefixed or a named keep.
func protectedHome(name string) bool {
	return strings.HasPrefix(name, ".") || slices.Contains(homeKept, name)
}

// scanBusy returns root-relative process working directories and open files.
func scanBusy(opts options) ([]string, error) {
	busy, err := busyRelative(opts.procDir, opts.root, opts.readCwd)
	if err != nil {
		return nil, err
	}
	open, err := openFilesRelative(opts.procDir, opts.root)
	return append(busy, open...), err
}

// openFilesRelative returns the root-relative files that live processes hold open.
// A process that exits during the scan is skipped; any other read error is returned.
func openFilesRelative(procDir, root string) ([]string, error) {
	pids, err := os.ReadDir(procDir)
	if err != nil {
		return nil, fmt.Errorf("read process table: %w", err)
	}
	var open []string
	for _, pid := range pids {
		if !isPID(pid.Name()) {
			continue
		}
		fdDir := filepath.Join(procDir, pid.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if exitedProcess(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read open files of pid %s: %w", pid.Name(), err)
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if exitedProcess(err) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("read fd %s of pid %s: %w", fd.Name(), pid.Name(), err)
			}
			if rel, ok := relativeUnder(root, target); ok {
				open = append(open, rel)
			}
		}
	}
	return open, nil
}
