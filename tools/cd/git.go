// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// gitRun runs git in dir with extra environment entries and returns trimmed stdout.
func gitRun(ctx context.Context, dir string, env []string, input []byte, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", args...) //nolint:gosec // Git is fixed; arguments are passed without a shell.
	command.Dir = dir
	command.Env = append(os.Environ(), env...)
	if input != nil {
		command.Stdin = bytes.NewReader(input)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	return gitRun(ctx, dir, nil, nil, args...)
}

// isAncestor reports whether ancestor is reachable from descendant.
func isAncestor(ctx context.Context, dir, ancestor, descendant string) (bool, error) {
	command := exec.CommandContext(ctx, "git", "merge-base", "--is-ancestor", ancestor, descendant) //nolint:gosec // Fixed binary, no shell.
	command.Dir = dir
	err := command.Run()
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git merge-base --is-ancestor %s %s: %w", ancestor, descendant, err)
}

func lines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
