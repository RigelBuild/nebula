// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	os.Exit(run(context.Background(), os.Getenv, os.Stdout, os.Stderr))
}

func run(ctx context.Context, getenv func(string) string, stdout, stderr io.Writer) int {
	command := newRootCommand(ctx, getenv, stdout, stderr)
	if err := command.ExecuteContext(ctx); err != nil {
		exitErr, ok := errors.AsType[exitCodeError](err)
		if ok {
			if _, writeErr := fmt.Fprintln(stderr, exitErr.message); writeErr != nil {
				slog.New(slog.NewTextHandler(stderr, nil)).Error("writing rigel-backup-sync error", "error", writeErr)
				return 1
			}
			return exitErr.code
		}
		slog.New(slog.NewTextHandler(stderr, nil)).Error("rigel-backup-sync failed", "error", err)
		return 1
	}
	return 0
}

type exitCodeError struct {
	code    int
	message string
}

func (e exitCodeError) Error() string { return e.message }

func (e exitCodeError) ExitCode() int { return e.code }

func newRootCommand(ctx context.Context, getenv func(string) string, stdout, stderr io.Writer) *cobra.Command {
	command := &cobra.Command{
		Use:   "rigel-backup-sync",
		Short: "Fast-forward the local mirror of the upstream main branch",
		Long: "Fast-forwards the local mirror of the upstream main branch.\n\n" +
			"Env: RIGEL_BACKUP_KEY_FILE, RIGEL_BACKUP_KNOWN_HOSTS, RIGEL_BACKUP_MIRROR, RIGEL_BACKUP_UPSTREAM.\n" +
			"Exit codes: 0 success; 1 fetch or mirror update failure; 2 missing configuration.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(_ *cobra.Command, _ []string) error {
			config, err := readSyncConfig(getenv)
			if err != nil {
				return exitCodeError{code: 2, message: err.Error()}
			}
			result := sync(ctx, config)
			if result.code != 0 {
				return exitCodeError{code: result.code, message: result.line}
			}
			if _, err := fmt.Fprintln(stdout, result.line); err != nil {
				return fmt.Errorf("writing sync result: %w", err)
			}
			return nil
		},
	}
	command.CompletionOptions.DisableDefaultCmd = true
	command.SetOut(stdout)
	command.SetErr(stderr)
	return command
}
