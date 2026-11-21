package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec" //nolint:depguard // Fixed executables run with argv and no shell.
	"strings"
)

// commandResult is what a finished command printed. Stderr is kept apart so a caller can read jj's warnings.
type commandResult struct {
	stdout string
	stderr string
}

// commandRun runs name in dir with argv and no shell. A nil env inherits the process environment; a non-nil env
// replaces it, so a test can pin HOME and JJ_CONFIG for one child without touching the parent. A non-zero exit is an error.
func commandRun(ctx context.Context, dir string, env []string, name string, args ...string) (commandResult, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // Fixed executable, argv without a shell.
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := commandResult{stdout: stdout.String(), stderr: stderr.String()}
	if err != nil {
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return result, fmt.Errorf("%s: %w: %s", name, err, detail)
		}
		return result, fmt.Errorf("%s: %w", name, err)
	}
	return result, nil
}
