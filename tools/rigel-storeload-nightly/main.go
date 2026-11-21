// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

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
				return 1
			}
			return exitErr.code
		}
		slog.New(slog.NewTextHandler(stderr, nil)).Error("rigel-storeload-nightly failed", "error", err)
		return 1
	}
	return 0
}

type exitCodeError struct {
	code    int
	message string
}

func (e exitCodeError) Error() string { return e.message }

func newRootCommand(ctx context.Context, getenv func(string) string, stdout, stderr io.Writer) *cobra.Command {
	command := &cobra.Command{
		Use:   "rigel-storeload-nightly",
		Short: "Run the nightly Rigel store-load latency benchmark",
		Long: "Runs the store-load latency benchmark, reports results to Linear, and posts failures to Cotal.\n\n" +
			"Env: RIGEL_NIGHTLY_SOURCE, RIGEL_NIGHTLY_CLONE, RIGEL_NIGHTLY_HOST, RIGEL_NIGHTLY_TASK, RIGEL_NIGHTLY_ISSUE, RIGEL_NIGHTLY_MENTION, RIGEL_NIGHTLY_COTAL_CHANNEL, RIGEL_NIGHTLY_COTAL_DIR, LINEAR_CLIENT_ID_FILE, LINEAR_CLIENT_SECRET_FILE, PATH.\n" +
			"Exit codes: 0 when the run and reports succeed; 1 when configuration, benchmark, or reporting fails.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(_ *cobra.Command, _ []string) error {
			config, err := configFromEnvironment(getenv)
			if err != nil {
				return exitCodeError{code: 1, message: err.Error()}
			}
			deps := nightlyDeps{
				runner:    runSystemProcess,
				client:    &http.Client{Timeout: 10 * time.Second},
				linearURL: linearURL,
				now:       time.Now,
				sleep:     sleepContext,
				log: func(line string) {
					if _, err := fmt.Fprintln(stderr, line); err != nil {
						slog.New(slog.NewTextHandler(stderr, nil)).Error("writing nightly log", "error", err)
					}
				},
			}
			result := runNightly(ctx, config, deps)
			if err := writeResult(stdout, stderr, result); err != nil {
				return err
			}
			if result.exitCode != 0 {
				return exitCodeError{code: result.exitCode, message: "nightly run failed"}
			}
			return nil
		},
	}
	command.CompletionOptions.DisableDefaultCmd = true
	command.SetOut(stdout)
	command.SetErr(stderr)
	return command
}
