package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec" //nolint:depguard // WSL commands run directly with fixed executables and argv.
	"path/filepath"

	"github.com/RigelBuild/nebula/tools/windows-agent-sync/internal/envstore"
	"github.com/samber/oops"
	"github.com/spf13/cobra"
)

// defaultDistro is the NixOS-WSL distribution that holds the secrets.
const defaultDistro = "NixOS"

type environment func(string) string

type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

type EnvStore = envstore.Store

type state struct {
	Managed []string `json:"managed"`
}

type storeFactory func() (EnvStore, error)

func main() {
	os.Exit(run(context.Background(), os.Getenv, os.Stdout, os.Stderr))
}

func run(ctx context.Context, getenv environment, stdout, stderr io.Writer) int {
	return runWith(ctx, getenv, stdout, stderr, execRunner, envstore.New)
}

func runWith(ctx context.Context, getenv environment, stdout, stderr io.Writer, runner Runner, createStore storeFactory) int {
	command := newRootCommand(ctx, getenv, stdout, stderr, runner, createStore)
	if err := command.Execute(); err != nil {
		slog.New(slog.NewTextHandler(stderr, nil)).Error("windows-agent-sync failed", "error", err)
		return 1
	}
	return 0
}

func newRootCommand(ctx context.Context, getenv environment, stdout, stderr io.Writer, runner Runner, createStore storeFactory) *cobra.Command {
	return newRootCommandWithStateWriter(
		ctx,
		getenv,
		stdout,
		stderr,
		runner,
		createStore,
		func(co checkout, home, appData string) error {
			table, err := loadLinks(co.path(co.layout.LinkTable))
			if err != nil {
				return oops.In("windows-agent-sync").Wrapf(err, "loading link table")
			}
			if err := syncLinks(co, home, appData, table, osLinkFS{}); err != nil {
				return err
			}
			return syncMCP(co, home)
		},
		syncBinaries,
		writeState,
	)
}

func newRootCommandWithStateWriter(ctx context.Context, getenv environment, stdout, stderr io.Writer, runner Runner, createStore storeFactory, links func(co checkout, home, appData string) error, binaries func(context.Context, Runner, EnvStore, checkout, string) error, stateWriter func(string, []string) error) *cobra.Command {
	var repoRoot, layoutFile string
	command := &cobra.Command{
		Use:           "windows-agent-sync",
		Short:         "Sync Windows agent workstation links, binaries, and secrets",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if repoRoot == "" {
				return oops.In("windows-agent-sync").Errorf("--repo-root is required")
			}
			if layoutFile == "" {
				return oops.In("windows-agent-sync").Errorf("--layout is required")
			}
			layoutPaths, err := loadLayout(layoutFile)
			if err != nil {
				return oops.In("windows-agent-sync").Wrapf(err, "loading layout")
			}
			home := getenv("USERPROFILE")
			if home == "" {
				return oops.In("windows-agent-sync").Errorf("USERPROFILE is required")
			}
			appData := getenv("APPDATA")
			if appData == "" {
				return oops.In("windows-agent-sync").Errorf("APPDATA is required")
			}
			localAppData := getenv("LOCALAPPDATA")
			if localAppData == "" {
				return oops.In("windows-agent-sync").Errorf("LOCALAPPDATA is required")
			}
			co := checkout{root: repoRoot, layout: layoutPaths}
			if err := runAllPhases(ctx, runner, createStore, co, home, appData, localAppData, links, binaries, stateWriter); err != nil {
				return err
			}
			_, err = fmt.Fprintln(stdout, "windows-agent-sync: links, binaries, and secrets synchronized")
			return err
		},
	}
	command.Flags().StringVar(&repoRoot, "repo-root", "", "path to the Windows checkout that the layout's paths are relative to")
	command.Flags().StringVar(&layoutFile, "layout", "", "CUE file naming the repo-relative source paths")
	command.CompletionOptions.DisableDefaultCmd = true
	command.SetOut(stdout)
	command.SetErr(stderr)
	return command
}

func runAllPhases(ctx context.Context, runner Runner, createStore storeFactory, co checkout, home, appData, localAppData string, links func(co checkout, home, appData string) error, binaries func(context.Context, Runner, EnvStore, checkout, string) error, stateWriter func(string, []string) error) error {
	statePath := filepath.Join(localAppData, "rigel", "windows-agent-sync", "state.json")
	var store EnvStore
	var plan secretPlan
	runErr := runSyncPhases(
		func() error {
			if err := links(co, home, appData); err != nil {
				return oops.In("windows-agent-sync").Wrapf(err, "syncing links")
			}
			return nil
		},
		func() error {
			var err error
			store, err = createStore()
			if err != nil {
				return oops.In("windows-agent-sync").Wrapf(err, "opening Windows environment store")
			}
			if err := binaries(ctx, runner, store, co, home); err != nil {
				return oops.In("windows-agent-sync").Wrapf(err, "syncing binaries")
			}
			return nil
		},
		func() error {
			if store == nil {
				return oops.In("windows-agent-sync").Errorf("Windows environment store is unavailable")
			}
			var err error
			plan, err = runSecretsPhase(ctx, runner, store, co, localAppData, statePath, stateWriter)
			return err
		},
	)
	if store != nil {
		if closer, ok := store.(interface{ Close() error }); ok {
			if err := closer.Close(); err != nil {
				runErr = errors.Join(runErr, oops.In("windows-agent-sync").Wrapf(err, "closing Windows environment store"))
			}
		}
	}
	if runErr != nil {
		return runErr
	}
	if err := stateWriter(statePath, plan.names); err != nil {
		return oops.In("windows-agent-sync").Wrapf(err, "writing managed environment state")
	}
	return nil
}

// runSecretsPhase runs last, so links and binaries land even when the secret map
// cannot be evaluated (for example, before NixOS-WSL is activated).
func runSecretsPhase(ctx context.Context, runner Runner, store EnvStore, co checkout, localAppData, statePath string, stateWriter func(string, []string) error) (secretPlan, error) {
	mapPath, err := toWSLPath(co.path(co.layout.SecretMap))
	if err != nil {
		return secretPlan{}, oops.In("windows-agent-sync").Wrapf(err, "mapping secret map path")
	}
	managed, err := readState(statePath)
	if err != nil {
		return secretPlan{}, oops.In("windows-agent-sync").Wrapf(err, "reading managed environment state")
	}
	plan, err := prepareSecrets(ctx, runner, defaultDistro, mapPath, managed)
	if err != nil {
		return secretPlan{}, err
	}
	if err := stateWriter(statePath, plan.managed); err != nil {
		return secretPlan{}, oops.In("windows-agent-sync").Wrapf(err, "writing managed environment state before applying secrets")
	}
	if err := applySecretPlan(store, plan, filepath.Join(localAppData, "rigel", "secrets")); err != nil {
		return secretPlan{}, err
	}
	return plan, nil
}

func runSyncPhases(links, binaries, secrets func() error) error {
	for _, phase := range []struct {
		name string
		run  func() error
	}{
		{name: "links", run: links},
		{name: "binaries", run: binaries},
		{name: "secrets", run: secrets},
	} {
		if err := phase.run(); err != nil {
			return oops.In("windows-agent-sync").Wrapf(err, "syncing %s phase", phase.name)
		}
	}
	return nil
}

func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...) //nolint:gosec // Callers use fixed executables such as wsl.exe, cargo, and bun; argv bypasses shell parsing.
	output, err := command.Output()
	if err != nil {
		return nil, oops.In("windows-agent-sync").Wrapf(err, "running %s", name)
	}
	return output, nil
}

type secretPlan struct {
	vars       map[string]string
	files      map[string]string
	managed    []string
	oldManaged []string
	names      []string
}

func prepareSecrets(ctx context.Context, runner Runner, distro, mapPath string, managed []string) (secretPlan, error) {
	secretMap, err := loadMap(ctx, runner, distro, mapPath)
	if err != nil {
		return secretPlan{}, err
	}
	vars, files, err := resolve(ctx, runner, distro, secretMap)
	if err != nil {
		return secretPlan{}, err
	}
	names := unionNames(sortedKeys(vars), sortedKeys(files))
	return secretPlan{vars: vars, files: files, managed: unionNames(managed, names), oldManaged: managed, names: names}, nil
}

func applySecretPlan(store EnvStore, plan secretPlan, fileDir string) error {
	_, err := applySecrets(store, plan.vars, plan.files, fileDir, plan.oldManaged)
	return err
}

func unionNames(groups ...[]string) []string {
	unique := make(map[string]struct{})
	for _, group := range groups {
		for _, name := range group {
			unique[name] = struct{}{}
		}
	}
	return sortedKeys(unique)
}

func readState(path string) ([]string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is state.json under %LOCALAPPDATA%/rigel/windows-agent-sync at runtime.
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, oops.In("windows-agent-sync").Wrapf(err, "reading state file")
	}
	var saved state
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil, oops.In("windows-agent-sync").Wrapf(err, "decoding state file")
	}
	if saved.Managed == nil {
		saved.Managed = []string{}
	}
	for _, name := range saved.Managed {
		if !validVarName(name) {
			return nil, oops.In("windows-agent-sync").Errorf("state file variable %q must match %s", name, varNamePattern)
		}
	}
	return saved.Managed, nil
}

func writeState(path string, managed []string) error {
	return writeStateWithReplace(path, managed, envstore.ReplaceFile)
}

func writeStateWithReplace(path string, managed []string, replace func(src, dst string) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return oops.In("windows-agent-sync").Wrapf(err, "creating state directory")
	}
	data, err := json.Marshal(state{Managed: managed})
	if err != nil {
		return oops.In("windows-agent-sync").Wrapf(err, "encoding state")
	}
	current, err := os.ReadFile(path) //nolint:gosec // path is state.json under %LOCALAPPDATA%/rigel/windows-agent-sync at runtime.
	if err == nil && bytes.Equal(current, data) {
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return oops.In("windows-agent-sync").Wrapf(err, "reading current state")
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".state-*.tmp")
	if err != nil {
		return oops.In("windows-agent-sync").Wrapf(err, "staging state file")
	}
	tempPath := file.Name()
	writeErr := file.Chmod(0o600)
	if writeErr == nil {
		_, writeErr = file.Write(data)
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		removeErr := os.Remove(tempPath)
		return oops.In("windows-agent-sync").Wrapf(errors.Join(writeErr, closeErr, removeErr), "staging state file")
	}
	if err := replace(tempPath, path); err != nil {
		removeErr := os.Remove(tempPath)
		return oops.In("windows-agent-sync").Wrapf(errors.Join(err, removeErr), "committing state file")
	}
	return nil
}
