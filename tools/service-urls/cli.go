// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/RigelBuild/nebula/tools/service-urls/config"
	"github.com/spf13/cobra"
)

// Main runs the service-urls command with args and returns the exit status.
func Main(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	command := newRootCommand(ctx, getenv, stdout, stderr)
	command.SetArgs(args)
	if err := command.Execute(); err != nil {
		status, ok := errors.AsType[commandStatusError](err)
		if ok {
			return status.code
		}
		slog.New(slog.NewTextHandler(stderr, nil)).Error("service-urls failed", "error", err)
		return 2
	}
	return 0
}

func newRootCommand(ctx context.Context, getenv func(string) string, stdout, stderr io.Writer) *cobra.Command {
	var configPath string
	command := &cobra.Command{
		Use:   "service-urls",
		Short: "Generate and check the service URL inventory.",
		Long: "Generates the service links page and Glance bookmarks from the service URL inventory.\n\n" +
			"Required flag:\n" +
			"  --config  Path to the deployment CUE config (see config/schema.cue).\n\n" +
			"Environment for repo-inventory:\n" +
			"  REPO_INVENTORY_APP_PRIVATE_KEY  Required GitHub App private key PEM.\n" +
			"  REPO_INVENTORY_APP_CLIENT_ID    Required GitHub App client ID.\n" +
			"  REPO                             Required repository in owner/name form.\n\n" +
			"Exit codes: 0 success, 1 inventory gaps, 2 operational error.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			root, err := workspaceRoot(cfg.InventoryPath)
			if err != nil {
				return err
			}
			return generate(root, cfg)
		},
	}
	command.PersistentFlags().StringVar(&configPath, "config", "", "path to the deployment CUE config (required)")
	command.MarkPersistentFlagRequired("config")
	command.CompletionOptions.DisableDefaultCmd = true
	command.SetOut(stdout)
	command.SetErr(stderr)
	command.AddCommand(newCoverageCommand(&configPath, stdout, stderr))
	command.AddCommand(newRepoInventoryCommand(ctx, getenv, &configPath, stdout, stderr))
	return command
}

func newCoverageCommand(configPath *string, stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "coverage",
		Short: "Check service URL coverage against deployed endpoints.",
		Long: "Checks inventory rows against Kubernetes, internal DNS, Cloudflare, and Worker endpoints.\n\n" +
			"Exit codes: 0 complete coverage, 1 inventory gaps, 2 operational error.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.Load(*configPath)
			if err != nil {
				return err
			}
			root, err := workspaceRoot(cfg.InventoryPath)
			if err != nil {
				return err
			}
			if code := runCoverage(root, cfg, stdout, stderr); code != 0 {
				return commandStatusError{code: code}
			}
			return nil
		},
	}
}

func newRepoInventoryCommand(ctx context.Context, getenv func(string) string, configPath *string, stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "repo-inventory",
		Short: "Compare the GitHub organization against its managed repository inventory.",
		Long: "Checks active GitHub organization repositories against the managed repositories file.\n\n" +
			"Environment:\n" +
			"  REPO_INVENTORY_APP_PRIVATE_KEY  Required GitHub App private key PEM.\n" +
			"  REPO_INVENTORY_APP_CLIENT_ID    Required GitHub App client ID.\n" +
			"  REPO                             Required repository in owner/name form.\n\n" +
			"Exit codes: 0 inventories match, 1 inventory gaps, 2 operational error.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.Load(*configPath)
			if err != nil {
				return err
			}
			root, err := workspaceRoot(cfg.InventoryPath)
			if err != nil {
				return err
			}
			code := runRepoInventory(ctx, root, cfg, getenv, stdout, stderr)
			if code != 0 {
				return commandStatusError{code: code}
			}
			return nil
		},
	}
}

// workspaceRoot finds the nearest ancestor of the working directory that holds
// groups.toml under inventoryPath, which comes from the deployment config.
func workspaceRoot(inventoryPath string) (string, error) {
	root, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("find workspace root: %w", err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	for {
		marker := filepath.Join(root, filepath.FromSlash(inventoryPath), "groups.toml")
		if _, err := os.Stat(marker); err == nil {
			return root, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("inspect workspace root: %w", err)
		}
		parent := filepath.Dir(root)
		if parent == root {
			return "", fmt.Errorf("find workspace root: %s/groups.toml not found", inventoryPath)
		}
		root = parent
	}
}

type commandStatusError struct {
	code int
}

func (e commandStatusError) Error() string {
	return fmt.Sprintf("command exited with status %d", e.code)
}

func writeLine(writer io.Writer, line string) bool {
	_, err := io.WriteString(writer, line+"\n")
	return err == nil
}

func writeFailure(writer io.Writer, line string) int {
	writeLine(writer, line)
	return 2
}
