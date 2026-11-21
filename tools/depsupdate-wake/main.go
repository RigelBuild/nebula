package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec" //nolint:depguard // This runner is the sole subprocess boundary and is behind Runner for tests.
	"path/filepath"
	"strings"
	"time"
)

const (
	inflightFile = "depsupdate.inflight"
	lastAllFile  = "depsupdate.last-all"
	allInterval  = 24 * time.Hour
)

type environment func(string) string

type Runner interface {
	Run(ctx context.Context, dir string, name string, args ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...) //nolint:gosec // Executables are fixed at call sites; args bypass shell parsing.
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("running %s: %w", name, err)
	}
	return output, nil
}

func main() {
	if err := decide(context.Background(), os.Getenv, execRunner{}, time.Now); err != nil {
		slog.Error("depsupdate-wake failed", "err", err)
		os.Exit(1)
	}
}

func decide(ctx context.Context, getenv environment, runner Runner, now func() time.Time) error {
	clone := getenv("DEPSUPDATE_CLONE")
	stateDir := getenv("DEPSUPDATE_STATE_DIR")
	to := getenv("COTAL_TO")
	dir := getenv("COTAL_DIR")
	if clone == "" {
		return errors.New("DEPSUPDATE_CLONE is required")
	}
	if stateDir == "" {
		return errors.New("DEPSUPDATE_STATE_DIR is required")
	}
	if to == "" {
		return errors.New("COTAL_TO is required")
	}
	if dir == "" {
		return errors.New("COTAL_DIR is required")
	}

	marker := filepath.Join(stateDir, inflightFile)
	if _, err := os.Stat(marker); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checking in-flight marker: %w", err)
	}

	if _, err := runner.Run(ctx, "", "git", "-C", clone, "fetch", "--quiet", "origin", "main"); err != nil {
		return fmt.Errorf("fetching rigel main: %w", err)
	}
	if _, err := runner.Run(ctx, "", "git", "-C", clone, "checkout", "--quiet", "--detach", "--force", "FETCH_HEAD"); err != nil {
		return fmt.Errorf("checking out rigel main: %w", err)
	}

	lastAll, err := readLastAll(filepath.Join(stateDir, lastAllFile))
	if err != nil {
		return err
	}

	if lastAll.IsZero() || now().Sub(lastAll) > allInterval {
		return sendWakeIfNoInflight(ctx, runner, dir, stateDir, to, "depsupdate-wake: all")
	}

	output, gateErr := runner.Run(ctx, clone, "devenv", "shell", "--", "go", "run", "./tools/vuln-gate")
	if gateErr == nil {
		return nil
	}

	wake := "depsupdate-wake: security\n" + tail(output, 20)
	return sendWakeIfNoInflight(ctx, runner, dir, stateDir, to, wake)
}

func readLastAll(path string) (time.Time, error) {
	contents, err := os.ReadFile(path) //nolint:gosec // This path is a fixed state filename under the configured state directory.
	if errors.Is(err, os.ErrNotExist) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("reading last all timestamp: %w", err)
	}

	lastAll, err := time.Parse(time.RFC3339, strings.TrimSpace(string(contents)))
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing last all timestamp: %w", err)
	}
	return lastAll, nil
}

func sendWakeIfNoInflight(ctx context.Context, runner Runner, dir, stateDir, to, text string) error {
	marker := filepath.Join(stateDir, inflightFile)
	if _, err := os.Stat(marker); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("rechecking in-flight marker: %w", err)
	}
	return sendWake(ctx, runner, dir, to, text)
}

func sendWake(ctx context.Context, runner Runner, dir, to, text string) error {
	if _, err := runner.Run(
		ctx,
		dir,
		"cotal",
		"send",
		"--to",
		to,
		text,
		"--space",
		"main",
	); err != nil {
		return fmt.Errorf("sending wake to %q: %w", to, err)
	}
	return nil
}

func tail(output []byte, limit int) string {
	lines := strings.Split(strings.TrimRight(string(output), "\r\n"), "\n")
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	return strings.Join(lines, "\n")
}
