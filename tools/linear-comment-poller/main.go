package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/credentialfile"
	"github.com/RigelBuild/nebula/tools/go-lib/linearauth"
	"github.com/spf13/cobra"
)

//go:embed contract.json
var contractJSON []byte

func main() { os.Exit(run(context.Background(), os.Getenv, os.Stdout, os.Stderr)) }

type environment func(string) string

func run(ctx context.Context, getenv environment, stdout, stderr io.Writer) int {
	return runWithArgs(ctx, getenv, nil, stdout, stderr)
}

func runWithArgs(ctx context.Context, getenv environment, args []string, stdout, stderr io.Writer) int {
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	command, err := newRootCommand(ctx, getenv, stdout, stderr, logger)
	if err != nil {
		logger.Error("loading poller contract", "error", err)
		return 1
	}
	if args != nil {
		command.SetArgs(args)
	}
	if err := command.ExecuteContext(ctx); err != nil {
		if commandExit, ok := errors.AsType[exitCodeError](err); ok {
			return commandExit.code
		}
		logger.Error("linear-comment-poller failed", "error", err)
		return 1
	}
	return 0
}

func loadContract() (Contract, error) {
	var contract Contract
	if err := json.Unmarshal(contractJSON, &contract); err != nil {
		return Contract{}, fmt.Errorf("decoding embedded poller contract: %w", err)
	}
	if contract.SuccessPrefix == "" || contract.StaleAfterMS <= 0 || contract.FallbackAfterMS <= 0 {
		return Contract{}, errors.New("embedded poller contract has invalid values")
	}
	return contract, nil
}

func newRootCommand(ctx context.Context, getenv environment, stdout, stderr io.Writer, logger *slog.Logger) (*cobra.Command, error) {
	contract, err := loadContract()
	if err != nil {
		return nil, err
	}
	var issuePrefixFlags []string
	var prefixes issuePrefixes
	root := &cobra.Command{
		Use:           "linear-comment-poller",
		Short:         "Fetch and read shared Linear comment caches",
		Long:          "Fetches watched Linear comments into a shared cache or reads a watchlist from the cache.\n\nRequired: --issue-prefix <KEY> (repeatable, or " + issuePrefixEnv + "=<KEY[,KEY...]>) names the Linear team keys to watch: the first is the team, and every later key is an alias that normalises to it.\nEnv: WAVE_WORKSPACES, LINEAR_COMMENTS_CACHE, LINEAR_COMMENTS_NOTIFY, LINEAR_CLIENT_ID_FILE, LINEAR_CLIENT_SECRET_FILE, LINEAR_OWN_USER_ID, LINEAR_AGENT_USER_ID_FILE, HOME.\nExit codes: read 0 fresh, 1 stale or cache failure, 2 bad watchlist, argv or issue prefix; sweep 0 success, 1 failure.",
		SilenceErrors: true,
		SilenceUsage:  true,
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			values := issuePrefixFlags
			if len(values) == 0 {
				values = []string{getenv(issuePrefixEnv)}
			}
			parsed, err := parseIssuePrefixes(values)
			if err != nil {
				logger.Error("invalid issue prefix", "error", err)
				return exitCodeError{code: 2}
			}
			prefixes = parsed
			return nil
		},
	}
	root.PersistentFlags().StringArrayVar(&issuePrefixFlags, "issue-prefix", nil, "Linear team key; repeatable, the first is the team and the rest are aliases (required unless "+issuePrefixEnv+" is set)")
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.AddCommand(newReadCommand(getenv, stdout, stderr, contract, &prefixes))
	root.AddCommand(newSweepCommand(ctx, getenv, stdout, stderr, logger, contract, &prefixes))
	return root, nil
}

func newReadCommand(getenv environment, stdout, stderr io.Writer, contract Contract, prefixes *issuePrefixes) *cobra.Command {
	var watchlistPath string
	command := &cobra.Command{
		Use:   "read",
		Short: "Read comments from the shared cache",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return exitCodeError{code: 2}
			}
			return nil
		},
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return exitCodeError{code: runRead([]string{"--watchlist", watchlistPath}, cachePath(getenv), time.Now(), stdout, stderr, contract, *prefixes)}
		},
	}
	command.Flags().StringVar(&watchlistPath, "watchlist", "", "Path to the watchlist JSON file")
	command.SetFlagErrorFunc(func(_ *cobra.Command, _ error) error { return exitCodeError{code: 2} })
	return command
}

type exitCodeError struct{ code int }

func (e exitCodeError) Error() string { return fmt.Sprintf("command exited with status %d", e.code) }

func newSweepCommand(ctx context.Context, getenv environment, stdout, stderr io.Writer, logger *slog.Logger, contract Contract, prefixes *issuePrefixes) *cobra.Command {
	command := &cobra.Command{
		Use:           "sweep",
		Short:         "Fetch watched Linear comments and update the cache",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(_ *cobra.Command, _ []string) error {
			deps := sweepDependencies(getenv, stdout, stderr, *prefixes)
			return exitCodeError{code: runSweep(ctx, deps, logger, contract)}
		},
	}
	return command
}

func sweepDependencies(getenv environment, stdout, stderr io.Writer, prefixes issuePrefixes) SweepDeps {
	home := getenv("HOME")
	if home == "" {
		home = "~"
	}
	workspacesRoot := getenv("WAVE_WORKSPACES")
	if workspacesRoot == "" {
		workspacesRoot = filepath.Join(home, "agents", "workspaces")
	}
	cachePath := cachePath(getenv)
	notifyPath := getenv("LINEAR_COMMENTS_NOTIFY")
	if notifyPath == "" {
		notifyPath = filepath.Join(home, "agents", "state", "linear-comments-notify.json")
	}
	clientID, hasClientID := credentialfile.Read(getenv("LINEAR_CLIENT_ID_FILE"))
	clientSecret, hasClientSecret := credentialfile.Read(getenv("LINEAR_CLIENT_SECRET_FILE"))
	var credentials *linearauth.Credentials
	if hasClientID && hasClientSecret {
		credentials = &linearauth.Credentials{ClientID: clientID, ClientSecret: clientSecret}
	}
	return SweepDeps{
		IssuePrefixes:   prefixes,
		WorkspacesRoot:  workspacesRoot,
		CachePath:       cachePath,
		NotifyPath:      notifyPath,
		Credentials:     credentials,
		OwnUserID:       getenv("LINEAR_OWN_USER_ID"),
		AgentsAppUserID: credentialValue(getenv("LINEAR_AGENT_USER_ID_FILE")),
		Client:          http.DefaultClient,
		Now:             time.Now,
		RunCotal:        runCotal,
		Stdout:          stdout,
		Stderr:          stderr,
	}
}

func cachePath(getenv environment) string {
	if path := getenv("LINEAR_COMMENTS_CACHE"); path != "" {
		return path
	}
	home := getenv("HOME")
	if home == "" {
		home = "~"
	}
	return filepath.Join(home, "agents", "state", "linear-comments.json")
}

func credentialValue(path string) string {
	value, ok := credentialfile.Read(path)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}
