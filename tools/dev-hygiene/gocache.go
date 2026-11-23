package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// goBuildDir is the one cache root that gets a size cap, trimmed item by item like Go's own trim.
const goBuildDir = "go-build"

// goCacheDirs are the cache roots under the user cache dir that the Go toolchain and linters fill.
var goCacheDirs = []string{goBuildDir, "golangci-lint", "staticcheck"}

// pruneGoCaches deletes regular files under each cache root whose mtime is older than cutoff.
// The tools touch an entry's mtime when they reuse it, so an old mtime means an unused entry.
// After the age pass, go-build is also capped at maxBytes, oldest items first (see capGoBuild).
func pruneGoCaches(ctx context.Context, cacheDir string, cutoff time.Time, maxBytes int64, apply bool, out *printer) error {
	var errs []error
	for _, name := range goCacheDirs {
		if err := ctx.Err(); err != nil {
			return err
		}
		removed, err := pruneTree(ctx, filepath.Join(cacheDir, name), cutoff, apply)
		if err != nil {
			errs = append(errs, fmt.Errorf("prune %s: %w", name, err))
		}
		out.printf("go cache %s: %d stale file(s)%s\n", name, removed, applyNote(apply))
		if name != goBuildDir {
			continue
		}
		items, freed, err := capGoBuild(ctx, filepath.Join(cacheDir, name), maxBytes, apply)
		if err != nil {
			errs = append(errs, fmt.Errorf("cap %s: %w", name, err))
		}
		out.printf("go cache %s: %d item(s) over the %d-byte cap, %d byte(s)%s\n", name, items, maxBytes, freed, applyNote(apply))
	}
	return errors.Join(errs...)
}

// pruneTree walks root and removes stale regular files. A missing root is not an error.
func pruneTree(ctx context.Context, root string, cutoff time.Time, apply bool) (int, error) {
	var removed int
	var errs []error
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			errs = append(errs, fmt.Errorf("stat %s: %w", path, err))
			return nil
		}
		if !info.ModTime().Before(cutoff) {
			return nil
		}
		if apply {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, fmt.Errorf("remove %s: %w", path, err))
				return nil
			}
		}
		removed++
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, fs.ErrNotExist) {
		errs = append(errs, walkErr)
	}
	return removed, errors.Join(errs...)
}

func applyNote(apply bool) string {
	if apply {
		return " removed"
	}
	return " (dry run)"
}

// goBuildItem is one file Go's build cache trims on its own: an <id>-a action file, an <outputID>-d
// output file, or an <outputID>-d output directory. An -a names its output by a different hash, and
// several actions can share one output, so items are not paired. Go treats a missing half as a miss.
type goBuildItem struct {
	path  string
	mtime time.Time
	size  int64
}

// capGoBuild removes go-build items, oldest mtime first, until the cache is at most maxBytes.
// It follows Go's trimSubdir (cmd/go/internal/cache/cache.go): every -a file, -d file and -d
// directory is its own item. Dry run only counts. It assumes no concurrent go run or go tool
// resolves an evicted -d at that moment; at the default cap eviction reaches only old items.
func capGoBuild(ctx context.Context, root string, maxBytes int64, apply bool) (int, int64, error) {
	items, total, err := scanGoBuild(ctx, root)
	if err != nil {
		return 0, 0, err
	}
	if total <= maxBytes {
		return 0, 0, nil
	}
	slices.SortFunc(items, func(a, b goBuildItem) int { return a.mtime.Compare(b.mtime) })
	var removed int
	var freed int64
	var errs []error
	for _, it := range items {
		if total <= maxBytes {
			break
		}
		if err := ctx.Err(); err != nil {
			return removed, freed, errors.Join(append(errs, err)...)
		}
		if apply {
			// RemoveAll unlinks a symlink inside a -d directory without following it.
			if err := os.RemoveAll(it.path); err != nil {
				errs = append(errs, fmt.Errorf("remove %s: %w", it.path, err))
				continue
			}
		}
		removed++
		freed += it.size
		total -= it.size
	}
	return removed, freed, errors.Join(errs...)
}

// scanGoBuild lists every -a and -d item in the two-hex subdirs of root and the total size.
// Files at the root (README, trim.txt) and symlinks are never items. A missing root is an empty cache.
func scanGoBuild(ctx context.Context, root string) ([]goBuildItem, int64, error) {
	var items []goBuildItem
	var total int64
	subs, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	for _, sub := range subs {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		if !sub.IsDir() || !isHexPair(sub.Name()) {
			continue
		}
		dir := filepath.Join(root, sub.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			return nil, 0, err
		}
		for _, f := range files {
			name := f.Name()
			isA, isD := strings.HasSuffix(name, "-a"), strings.HasSuffix(name, "-d")
			if !isA && !isD {
				continue
			}
			if !f.Type().IsRegular() && !(isD && f.IsDir()) {
				continue
			}
			info, err := f.Info()
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				return nil, 0, err
			}
			path := filepath.Join(dir, name)
			size := info.Size()
			if f.IsDir() {
				if size, err = treeSize(path); err != nil {
					return nil, 0, err
				}
			}
			items = append(items, goBuildItem{path: path, mtime: info.ModTime(), size: size})
			total += size
		}
	}
	return items, total, nil
}

func isHexPair(name string) bool {
	if len(name) != 2 {
		return false
	}
	for _, c := range name {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// treeSize sums the regular files under a -d directory. WalkDir does not follow symlinks.
func treeSize(path string) (int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}
