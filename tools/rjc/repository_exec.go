package main

import (
	"context"
	"os"
	"os/exec" //nolint:depguard // Child processes use literal argument vectors.
	"path/filepath"
	"slices"
	"strings"
)

func (r *repository) childEnv(extra []string) []string {
	return childEnvironment(append(append([]string(nil), r.env...), extra...))
}

func (r *repository) execCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	if strings.ContainsRune(name, os.PathSeparator) {
		return exec.CommandContext(ctx, name, args...) //nolint:gosec // Arguments are passed directly.
	}
	for _, entry := range slices.Backward(r.env) {
		path, ok := strings.CutPrefix(entry, "PATH=")
		if !ok {
			continue
		}
		for _, dir := range filepath.SplitList(path) {
			// The overlay is test-only and uses absolute dirs; a relative or empty entry would resolve via the process PATH or cwd.
			if !filepath.IsAbs(dir) {
				continue
			}
			candidate := filepath.Join(dir, name)
			if resolved, err := exec.LookPath(candidate); err == nil {
				return exec.CommandContext(ctx, resolved, args...) //nolint:gosec // Arguments are passed directly.
			}
		}
		cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // Arguments are passed directly.
		cmd.Err = &exec.Error{Name: name, Err: exec.ErrNotFound}
		return cmd
	}
	return exec.CommandContext(ctx, name, args...) //nolint:gosec // Arguments are passed directly.
}
