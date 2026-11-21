package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec" //nolint:depguard // The gate runs fixed go and git commands with literal argument vectors.
	"strings"
)

// rootChecksGate names the refusal gate for a failing pre-push root-checks run.
const rootChecksGate = "root-checks"

// rootChecksFix tells the author how to clear a root-checks refusal, whichever check failed. dir is the
// profile's rootChecksDir.
func rootChecksFix(dir string) string {
	return "run rjc inside the devenv shell, which pins biome, rumdl and moon; fix the reported check (`biome check --write`, `rumdl check --fix`, or add the project to " + dir + "/project-registry.expected.json), commit, and rerun rjc submit"
}

// rootChecksResult is one root-checks run: its exit code and combined output.
type rootChecksResult struct {
	code   int
	output string
}

// runRootChecks runs the named root-checks in the head checkout at root. A nil rootChecks uses the real gate.
func (r *repository) runRootChecks(ctx context.Context, root string, names []string) (rootChecksResult, error) {
	if r.rootChecks != nil {
		return r.rootChecks(ctx, root, names)
	}
	cmd := r.execCommand(ctx, "go", "run", "./"+r.profile.RootChecksDir, "--checks", strings.Join(names, ",")) //nolint:gosec // The directory and names come from the repository profile.
	cmd.Dir = root
	cmd.Env = r.childEnv([]string{"GATE_ROOT=" + root, "GOWORK=off"})
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err == nil {
		return rootChecksResult{output: out.String()}, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() > 0 {
		return rootChecksResult{code: exit.ExitCode(), output: out.String()}, nil
	}
	return rootChecksResult{}, toolFailure("hooks", fmt.Errorf("run root-checks: %w", err))
}

// addHeadWorktree checks rev out into a new detached git worktree and returns its path. A worktree keeps .git,
// so git ls-files still lists the tracked files that root-checks sweeps. The caller removes it with
// removeHeadWorktree.
func (r *repository) addHeadWorktree(ctx context.Context, rev string) (string, error) {
	dir, err := os.MkdirTemp("", "rjc-root-checks-")
	if err != nil {
		return "", toolFailure("hooks", err)
	}
	if _, err := r.command(ctx, nil, nil, "git", "worktree", "add", "--detach", dir, rev); err != nil {
		return dir, toolFailure("hooks", fmt.Errorf("git worktree add %s: %w", rev, err))
	}
	return dir, nil
}

// removeHeadWorktree unregisters the worktree at dir, then deletes whatever remains. It runs on every path, so it
// ignores ctx cancellation: a cancelled submit must still leave no registered worktree behind. If the remove fails,
// it prunes the stale registration after the directory is gone.
func (r *repository) removeHeadWorktree(ctx context.Context, dir string) error {
	cleanup := context.WithoutCancel(ctx)
	_, removeErr := r.command(cleanup, nil, nil, "git", "worktree", "remove", "--force", dir)
	removeAllErr := os.RemoveAll(dir)
	if removeErr == nil {
		return removeAllErr
	}
	_, pruneErr := r.command(cleanup, nil, nil, "git", "worktree", "prune")
	return errors.Join(removeErr, removeAllErr, pruneErr)
}

// runP0RootChecks runs the profile's cheap root-checks against each owned head before a p0 skip pushes.
// It refuses a failing check with the check output. Passing checks log one line and let the push continue.
func (r *repository) runP0RootChecks(ctx context.Context, line []Change, log io.Writer) error {
	names := r.profile.SkipChecks
	if len(names) == 0 {
		return nil
	}
	for _, change := range line {
		if !change.Owned {
			continue
		}
		if err := r.checkHeadRootChecks(ctx, change.Tip, names, log); err != nil {
			return err
		}
	}
	return nil
}

// checkHeadRootChecks checks one head out into a worktree, runs the named checks there, and removes the worktree
// on every path.
func (r *repository) checkHeadRootChecks(ctx context.Context, rev string, names []string, log io.Writer) (err error) {
	dir, err := r.addHeadWorktree(ctx, rev)
	if err != nil {
		// A failed add left no registration to remove, so only the directory needs deleting.
		if removeErr := os.RemoveAll(dir); removeErr != nil {
			return errors.Join(err, removeErr)
		}
		return err
	}
	defer func() { err = errors.Join(err, r.removeHeadWorktree(ctx, dir)) }()
	result, err := r.runRootChecks(ctx, dir, names)
	if err != nil {
		return err
	}
	if result.code != 0 {
		return refusal("hooks", rootChecksGate, "root-checks failed in the head tree:\n"+strings.TrimRight(result.output, "\n"), rootChecksFix(r.profile.RootChecksDir))
	}
	if _, err := fmt.Fprintf(log, "root-checks: %s passed\n", strings.Join(names, ",")); err != nil {
		return toolFailure("hooks", err)
	}
	return nil
}
