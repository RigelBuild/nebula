package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/relaunch"
	"github.com/RigelBuild/nebula/tools/wave-supervisor-relaunch/seats"
	"github.com/spf13/cobra"
)

func run(ctx context.Context, args []string, getenv envGetter, stdout, stderr io.Writer) int {
	configPath, rest, err := takeConfigFlag(args)
	if err != nil {
		return writeErrorStatus(stderr, err, 2)
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return writeErrorStatus(stderr, err, 1)
	}
	args = rest
	deps, err := defaultDeps(cfg, getenv)
	if err != nil {
		return writeErrorStatus(stderr, err, 1)
	}
	deps.WriteLog = func(line string) error {
		_, err := fmt.Fprintln(stdout, line)
		return err
	}
	root := newRootCommand(ctx, deps, stdout, stderr)
	root.SetArgs(args)
	root.SetIn(strings.NewReader(""))
	if err := root.ExecuteContext(ctx); err != nil {
		return commandStatus(err)
	}
	return 0
}

// takeConfigFlag removes the one required "--config <path>" pair from args and returns the path
// with the remaining arguments in order. A missing, blank, or repeated --config is an error.
func takeConfigFlag(args []string) (string, []string, error) {
	path := ""
	seen := false
	rest := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		if args[index] != "--config" {
			rest = append(rest, args[index])
			continue
		}
		if seen || index+1 >= len(args) || strings.TrimSpace(args[index+1]) == "" {
			return "", nil, errors.New("--config <path> is required once")
		}
		path = args[index+1]
		seen = true
		index++
	}
	if !seen {
		return "", nil, errors.New("--config <path> is required")
	}
	return path, rest, nil
}

func writeErrorStatus(writer io.Writer, err error, status int) int {
	if _, writeErr := fmt.Fprintln(writer, err); writeErr != nil {
		return 1
	}
	return status
}

func commandStatus(err error) int {
	if status, ok := errors.AsType[*commandError](err); ok {
		return status.code
	}
	return 1
}

type commandError struct {
	code int
}

func (e *commandError) Error() string { return "command failed" }

func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) != 0 {
		if _, err := fmt.Fprintln(cmd.ErrOrStderr(), usage()); err != nil {
			return fmt.Errorf("write usage: %w", err)
		}
		return &commandError{code: 2}
	}
	return nil
}

func newRootCommand(ctx context.Context, deps runDeps, stdout, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:           "wave-supervisor-relaunch",
		Short:         "Recover or inspect the supervisor wave",
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          noArgs,
	}
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		if _, writeErr := fmt.Fprintln(cmd.ErrOrStderr(), usage()); writeErr != nil {
			return fmt.Errorf("write usage: %w", writeErr)
		}
		return &commandError{code: 2}
	})
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.AddCommand(newRunCommand(ctx, deps), newHoldCommand(deps), newReleaseCommand(deps), newRestartCommand(ctx, deps), newStopPostCommand(ctx, deps))
	return root
}

func newRunCommand(ctx context.Context, deps runDeps) *cobra.Command {
	return &cobra.Command{Use: "run", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		outcome := runSupervisor(ctx, deps)
		if err := deps.log(ctx, summaryLine(outcome)); err != nil {
			return fmt.Errorf("write summary: %w", err)
		}
		if seats.RunLeftSeatsFree(outcome.Outcome, outcome.Reason) {
			// The seat step never changes run's exit code. A failed seat log write goes to stderr only.
			seats.Run(ctx, seatStepDeps(ctx, deps, cmd.ErrOrStderr()))
		}
		if outcome.Outcome == outcomeFailed {
			return &commandError{code: 1}
		}
		return nil
	}}
}

func newHoldCommand(deps runDeps) *cobra.Command {
	var reason string
	var minutes int
	command := &cobra.Command{
		Use: reasonHold, Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(reason) == "" || minutes < 1 || minutes > 120 {
				if _, err := fmt.Fprintln(cmd.ErrOrStderr(), usage()); err != nil {
					return fmt.Errorf("write usage: %w", err)
				}
				return &commandError{code: 2}
			}
			now := deps.now()
			hold := relaunch.Hold{Reason: reason, Until: now.Add(time.Duration(minutes) * time.Minute).UTC().Format("2006-01-02T15:04:05.000Z")}
			if err := relaunch.WriteHold(deps.Paths.Hold, hold, deps.FS); err != nil {
				return fmt.Errorf("write hold: %w", err)
			}
			return nil
		},
	}
	command.Flags().StringVar(&reason, "reason", "", "Reason for the hold")
	command.Flags().IntVar(&minutes, "minutes", 30, "Hold duration in minutes (1-120)")
	return command
}

func newReleaseCommand(deps runDeps) *cobra.Command {
	return &cobra.Command{Use: "release", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if err := relaunch.ReleaseHold(deps.Paths.Hold, deps.FS); err != nil {
			return fmt.Errorf("release hold: %w", err)
		}
		return nil
	}}
}

func parseRestartArgs(args []string) (reason, identity string, valid bool) {
	reasonSet, identitySet := false, false
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--reason":
			if reasonSet || index+1 == len(args) || strings.HasPrefix(args[index+1], "--") {
				return "", "", false
			}
			index++
			reason = args[index]
			reasonSet = true
		case "--rss":
			if identitySet || index+1 == len(args) {
				return "", "", false
			}
			index++
			identity = args[index]
			if !identityRE.MatchString(identity) {
				return "", "", false
			}
			identitySet = true
		default:
			return "", "", false
		}
	}
	if !reasonSet || strings.TrimSpace(reason) == "" {
		return "", "", false
	}
	return reason, identity, true
}

func newRestartCommand(ctx context.Context, deps runDeps) *cobra.Command {
	return &cobra.Command{
		Use:                "request-zellij-restart",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			reason, rss, valid := parseRestartArgs(args)
			if !valid {
				if _, err := fmt.Fprintln(cmd.ErrOrStderr(), usage()); err != nil {
					return fmt.Errorf("write usage: %w", err)
				}
				return &commandError{code: 2}
			}
			request := relaunch.RestartRequest{At: deps.now().UTC().Format("2006-01-02T15:04:05.000Z"), Reason: reason, Cause: "wedge"}
			if rss != "" {
				request.Cause = restartRSS
				request.Identity = rss
			}
			if err := relaunch.WriteRestartRequest(deps.Paths.RestartRequest, request, deps.FS); err != nil {
				return fmt.Errorf("write restart request: %w", err)
			}
			started, err := deps.StartRelaunch(ctx)
			if err != nil || !started {
				return restartStartFailure(deps, err)
			}
			return nil
		},
	}
}

func restartStartFailure(deps runDeps, err error) error {
	releaseErr := relaunch.ReleaseRestartRequest(deps.Paths.RestartRequest, deps.FS)
	if releaseErr != nil {
		if err != nil {
			return errors.Join(fmt.Errorf("start relaunch: %w", err), releaseErr)
		}
		return fmt.Errorf("start relaunch: %w", releaseErr)
	}
	if err != nil {
		return fmt.Errorf("start relaunch: %w", err)
	}
	return &commandError{code: 1}
}

func newStopPostCommand(ctx context.Context, deps runDeps) *cobra.Command {
	return &cobra.Command{Use: "stop-post", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if line := stopPostLine(deps.ServiceResult, deps.ServiceExitStatus); line != "" {
			if err := deps.log(ctx, line); err != nil {
				return fmt.Errorf("write stop summary: %w", err)
			}
		}
		return nil
	}}
}

func usage() string {
	return "Usage: wave-supervisor-relaunch run | hold --reason <text> [--minutes <n>] | release | request-zellij-restart --reason <text> [--rss <pid:starttime>] | stop-post"
}
