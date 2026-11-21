package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// goCacheDirs are the cache roots under the user cache dir that the Go toolchain and linters fill.
var goCacheDirs = []string{"go-build", "golangci-lint", "staticcheck"}

// pruneGoCaches deletes regular files under each cache root whose mtime is older than cutoff.
// The tools touch an entry's mtime when they reuse it, so an old mtime means an unused entry.
func pruneGoCaches(ctx context.Context, cacheDir string, cutoff time.Time, apply bool, out *printer) error {
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
