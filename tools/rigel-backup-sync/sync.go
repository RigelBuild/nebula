// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec" //nolint:depguard // This tool invokes Git directly without a shell.
	"strings"
)

const successPrefix = "rigel-backup-sync: ok"

const canonicalRef = "refs/heads/main"

const fetchedRef = "refs/rigel-backup-sync/main"

const gitDirFlag = "--git-dir"

type syncConfig struct {
	mirror   string
	upstream string
	env      map[string]string
}

type syncResult struct {
	code int
	line string
}

type commandResult struct {
	code int
	out  string
	err  string
}

func git(ctx context.Context, args []string, env map[string]string) commandResult {
	command := exec.CommandContext(ctx, "git", args...) //nolint:gosec // G204: Git arguments come from this tool's fixed internal calls.
	command.Env = mergedEnvironment(os.Environ(), env)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	code := 0
	if err != nil {
		code = 1
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() >= 0 {
			code = exitErr.ExitCode()
		}
	}
	return commandResult{code: code, out: strings.TrimSpace(stdout.String()), err: strings.TrimSpace(stderr.String())}
}

func mergedEnvironment(base []string, overrides map[string]string) []string {
	values := make(map[string]string, len(base)+len(overrides))
	for _, entry := range base {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}
	maps.Copy(values, overrides)
	env := make([]string, 0, len(values))
	for key, value := range values {
		env = append(env, key+"="+value)
	}
	return env
}
func sync(ctx context.Context, config syncConfig) syncResult {
	fetch, err := fetchGitHubMain(ctx, config)
	if err != nil {
		return failure("fetch failed", err)
	}
	current := git(ctx, []string{
		gitDirFlag, config.mirror, "rev-parse", "--verify", "--quiet", canonicalRef,
	}, config.env)
	localRefMissing := current.code == 1
	if current.code != 0 && !localRefMissing {
		return failure("cannot read local main", fmt.Errorf("(%d): %s", current.code, current.err))
	}
	if localRefMissing {
		return initializeLocalMain(ctx, config, fetch.out)
	}
	if current.out == fetch.out {
		return success(current.out)
	}
	if err := verifyFastForward(ctx, config, current.out, fetch.out); err != nil {
		return failure("fast-forward check failed", err)
	}
	return updateLocalMain(ctx, config, fetch.out, current.out)
}

func fetchGitHubMain(ctx context.Context, config syncConfig) (commandResult, error) {
	fetch := git(ctx, []string{
		gitDirFlag, config.mirror,
		"fetch", "--no-tags", config.upstream,
		"+" + canonicalRef + ":" + fetchedRef,
	}, config.env)
	if fetch.code != 0 {
		return commandResult{}, gitFailure(fetch)
	}
	fetched := git(ctx, []string{
		gitDirFlag, config.mirror, "rev-parse", "--verify", "--quiet", fetchedRef,
	}, config.env)
	if fetched.code != 0 {
		return commandResult{}, fmt.Errorf("cannot read fetched main (%d): %s", fetched.code, fetched.err)
	}
	return fetched, nil
}

func verifyFastForward(ctx context.Context, config syncConfig, current, fetched string) error {
	ancestor := git(ctx, []string{
		gitDirFlag, config.mirror, "merge-base", "--is-ancestor", current, fetched,
	}, config.env)
	if ancestor.code == 1 {
		return fmt.Errorf("refusing non-fast-forward update: local main %s is not an ancestor of GitHub main %s", current, fetched)
	}
	if ancestor.code != 0 {
		return fmt.Errorf("cannot verify fast-forward (%d): %s", ancestor.code, ancestor.err)
	}
	return nil
}

func initializeLocalMain(ctx context.Context, config syncConfig, fetched string) syncResult {
	update := git(ctx, []string{
		gitDirFlag, config.mirror, "update-ref", canonicalRef, fetched, strings.Repeat("0", len(fetched)),
	}, config.env)
	if update.code != 0 {
		return failure("cannot initialize local main", gitFailure(update))
	}
	return success(fetched)
}

func updateLocalMain(ctx context.Context, config syncConfig, fetched, current string) syncResult {
	update := git(ctx, []string{
		gitDirFlag, config.mirror, "update-ref", canonicalRef, fetched, current,
	}, config.env)
	if update.code != 0 {
		return failure("local main update failed", gitFailure(update))
	}
	return success(fetched)
}
func gitFailure(result commandResult) error {
	message := result.err
	if message == "" {
		message = result.out
	}
	return fmt.Errorf("(%d): %s", result.code, message)
}

func failure(message string, detail error) syncResult {
	return syncResult{code: 1, line: fmt.Sprintf("rigel-backup-sync: %s: %v", message, detail)}
}

func success(revision string) syncResult {
	return syncResult{line: fmt.Sprintf("%s main=%s", successPrefix, revision)}
}

func required(getenv func(string) string, name string) (string, error) {
	value := getenv(name)
	if value == "" {
		return "", fmt.Errorf("%s is not set", name)
	}
	return value, nil
}

func readSyncConfig(getenv func(string) string) (syncConfig, error) {
	keyFile, err := required(getenv, "RIGEL_BACKUP_KEY_FILE")
	if err != nil {
		return syncConfig{}, err
	}
	knownHosts, err := required(getenv, "RIGEL_BACKUP_KNOWN_HOSTS")
	if err != nil {
		return syncConfig{}, err
	}
	mirror, err := required(getenv, "RIGEL_BACKUP_MIRROR")
	if err != nil {
		return syncConfig{}, err
	}
	upstream, err := required(getenv, "RIGEL_BACKUP_UPSTREAM")
	if err != nil {
		return syncConfig{}, err
	}
	return syncConfig{
		mirror:   mirror,
		upstream: upstream,
		env: map[string]string{
			"GIT_TERMINAL_PROMPT": "0",
			"GIT_SSH_COMMAND": "ssh -i " + keyFile +
				" -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes" +
				" -o UserKnownHostsFile=" + knownHosts + " -o BatchMode=yes",
		},
	}, nil
}
