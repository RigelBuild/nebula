package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-github/v93/github"
	"github.com/spf13/cobra"
)

type environment func(string) string

type commandConfig struct {
	root       string
	token      string
	apiURL     string
	repository string
	gitHub     *github.Client
}

func main() {
	if err := newRootCommand(context.Background(), os.Getenv, os.Stdout).Execute(); err != nil {
		slog.Error("fork-sync failed", "error", err)
		os.Exit(1)
	}
}

func newRootCommand(_ context.Context, getenv environment, stdout io.Writer) *cobra.Command {
	root := &cobra.Command{Use: "fork-sync", Short: "Import, sync, and export vendored forks", SilenceErrors: true, SilenceUsage: true}
	root.CompletionOptions.DisableDefaultCmd = true
	var rootPath string
	root.PersistentFlags().StringVar(&rootPath, "root", "", "Nebula repository root (defaults to the current Git worktree)")

	importCommand := &cobra.Command{Use: "import", Short: "Import a fork tree as a squashed commit", Args: cobra.NoArgs}
	var importName, fork, ref, upstream, upstreamBranch, importBase string
	importCommand.Flags().StringVar(&importName, "name", "", "Fork name")
	importCommand.Flags().StringVar(&fork, "fork", "", "Fork repository as RigelBuild/<repo> or a Git URL")
	importCommand.Flags().StringVar(&ref, "ref", "rigel-release", "Fork ref to import")
	importCommand.Flags().StringVar(&upstream, "upstream", "", "Upstream Git URL")
	importCommand.Flags().StringVar(&upstreamBranch, "upstream-branch", "", "Upstream branch")
	importCommand.Flags().StringVar(&importBase, "base", "", "Explicit upstream base commit when no merge-base exists")
	_ = importCommand.MarkFlagRequired("name")
	_ = importCommand.MarkFlagRequired("fork")
	_ = importCommand.MarkFlagRequired("upstream")
	_ = importCommand.MarkFlagRequired("upstream-branch")
	importCommand.RunE = func(command *cobra.Command, _ []string) error {
		config, err := configForCommand(command.Context(), rootPath, getenv)
		if err != nil {
			return err
		}
		return importFork(command.Context(), config.root, importOptions{Name: importName, Fork: fork, Ref: ref, Upstream: upstream, UpstreamBranch: upstreamBranch, Base: importBase})
	}

	syncCommand := &cobra.Command{Use: "sync", Short: "Sync upstream changes into a vendored fork", Args: cobra.NoArgs}
	var syncName string
	var syncAll, dryRun bool
	syncCommand.Flags().StringVar(&syncName, "name", "", "Fork name")
	syncCommand.Flags().BoolVar(&syncAll, "all", false, "Sync every imported fork")
	syncCommand.Flags().BoolVar(&dryRun, "dry-run", false, "Print each fork's sync plan without pushing or opening a pull request")
	syncCommand.RunE = func(command *cobra.Command, _ []string) error {
		if (syncName == "") == !syncAll {
			return errors.New("choose exactly one of --name or --all")
		}
		config, err := configForCommand(command.Context(), rootPath, getenv)
		if err != nil {
			return err
		}
		names := []string{syncName}
		if syncAll {
			names, err = listForks(config.root)
			if err != nil {
				return err
			}
		}
		for _, name := range names {
			if err := syncFork(command.Context(), forkConfig{Name: name, Root: config.root, GitHubToken: config.token, Repository: config.repository, Client: config.gitHub, Output: command.OutOrStdout()}, dryRun); err != nil {
				return fmt.Errorf("sync %s: %w", name, err)
			}
		}
		return nil
	}

	exportCommand := &cobra.Command{Use: "export", Short: "Export a nebula commit range to an outbox branch", Args: cobra.NoArgs}
	var exportName, exportRange, exportTopic, outbox string
	exportCommand.Flags().StringVar(&exportName, "name", "", "Fork name")
	exportCommand.Flags().StringVar(&exportRange, "range", "", "Nebula commit range in A..B form")
	exportCommand.Flags().StringVar(&exportTopic, "topic", "", "Outbox branch topic")
	exportCommand.Flags().StringVar(&outbox, "outbox", "", "Outbox Git URL (defaults to RigelBuild/<name>)")
	_ = exportCommand.MarkFlagRequired("name")
	_ = exportCommand.MarkFlagRequired("range")
	_ = exportCommand.MarkFlagRequired("topic")
	exportCommand.RunE = func(command *cobra.Command, _ []string) error {
		config, err := configForCommand(command.Context(), rootPath, getenv)
		if err != nil {
			return err
		}
		return exportFork(command.Context(), config.root, exportOptions{Name: exportName, Range: exportRange, Topic: exportTopic, Outbox: outbox})
	}
	root.AddCommand(importCommand, syncCommand, exportCommand)
	root.SetOut(stdout)
	root.SetErr(stdout)
	return root
}

func configForCommand(ctx context.Context, rootPath string, getenv environment) (commandConfig, error) {
	root, err := resolveRoot(ctx, rootPath)
	if err != nil {
		return commandConfig{}, err
	}
	repository := getenv("GITHUB_REPOSITORY")
	if repository == "" {
		repository = "RigelBuild/nebula"
	}
	apiURL := strings.TrimRight(getenv("GITHUB_API_URL"), "/")
	if apiURL == "" {
		apiURL = "https://api.github.com"
	}

	token := getenv("GITHUB_TOKEN")
	baseURL := apiURL + "/"
	options := []github.ClientOptionsFunc{github.WithHTTPClient(&http.Client{}), github.WithURLs(&baseURL, nil)}
	if token != "" {
		options = append(options, github.WithAuthToken(token))
	}
	client, err := github.NewClient(options...)
	if err != nil {
		return commandConfig{}, fmt.Errorf("configure GitHub client: %w", err)
	}
	return commandConfig{root: root, token: token, apiURL: apiURL, repository: repository, gitHub: client}, nil
}

func resolveRoot(ctx context.Context, configured string) (string, error) {
	if configured != "" {
		return filepath.Abs(configured)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	root, err := git(ctx, cwd, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("find repository root: %w", err)
	}
	return strings.TrimSpace(string(root)), nil
}
