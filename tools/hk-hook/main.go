// Command hk-hook installs the hk pre-push hook into the git config of the current checkout.
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec" //nolint:depguard // Git and jj run with fixed argv, never through a shell.
	"slices"
	"strings"
)

// hookCommand runs on every push. HK=0 skips the gate.
const hookCommand = `test "${HK:-1}" = "0" || direnv exec . hk run pre-push --from-hook "$@"`

func main() {
	os.Exit(run(context.Background(), os.Stderr))
}

// run installs the hook into the current directory and returns the exit code.
func run(ctx context.Context, stderr io.Writer) int {
	repo, err := os.Getwd()
	if err == nil {
		err = install(ctx, repo)
	}
	if err != nil {
		// Exit 1 is already decided; a failed stderr write cannot change it.
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// install writes the hook settings into the git config that repo uses.
// --replace-all keeps a reinstall to one hook command.
func install(ctx context.Context, repo string) error {
	gitDir, err := gitDirArgs(ctx, repo)
	if err != nil {
		return err
	}
	settings := [...][2]string{
		{"hook.hk-pre-push.command", hookCommand},
		{"hook.hk-pre-push.event", "pre-push"},
	}
	for _, setting := range settings {
		args := slices.Concat(gitDir, []string{"config", "--local", "--replace-all", setting[0], setting[1]})
		if _, stderr, err := runIn(ctx, repo, "git", args...); err != nil {
			return fmt.Errorf("git config %s: %s: %w", setting[0], strings.TrimSpace(stderr), err)
		}
	}
	return nil
}

// gitDirArgs returns no flags for a git checkout, or --git-dir for a jj secondary workspace.
func gitDirArgs(ctx context.Context, repo string) ([]string, error) {
	// A failed rev-parse means no git dir here; fall through to jj, which reports its own failure.
	if _, _, err := runIn(ctx, repo, "git", "rev-parse", "--git-dir"); err == nil {
		return nil, nil
	}
	root, stderr, err := runIn(ctx, repo, "jj", "git", "root")
	if err != nil {
		return nil, fmt.Errorf("not a git repo or jj workspace: %s: %w", strings.TrimSpace(stderr), err)
	}
	return []string{"--git-dir=" + strings.TrimSpace(root)}, nil
}

// runIn runs name with args in dir and returns its stdout and stderr.
func runIn(ctx context.Context, dir, name string, args ...string) (string, string, error) {
	var stdout, stderr bytes.Buffer
	command := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: git and jj get fixed argv; no shell parses it.
	command.Dir = dir
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}
