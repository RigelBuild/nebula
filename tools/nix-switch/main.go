// SPDX-License-Identifier: MIT OR Apache-2.0

// Package main switches a nix-darwin host from a jj working copy.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	// Subprocesses are the purpose of this command.
	"os/exec" //nolint:depguard // The CLI delegates to sudo, jj, and nh.
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

type command struct {
	name           string
	args           []string
	dir            string
	env            []string
	stdin          io.Reader
	stdout, stderr io.Writer
}

type runner interface {
	run(ctx context.Context, c command) error
}
type execRunner struct{}

func (execRunner) run(ctx context.Context, c command) error {
	cmd := exec.CommandContext(ctx, c.name, c.args...) //nolint:gosec // Executables are fixed by the caller and arguments bypass shell parsing.
	cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = c.dir, c.stdin, c.stdout, c.stderr
	// The terminal sends SIGINT to the whole process group.
	cmd.Cancel = func() error { return nil }
	if len(c.env) > 0 {
		cmd.Env = append(os.Environ(), c.env...)
	}
	terms := make(chan os.Signal, 1)
	signal.Notify(terms, syscall.SIGTERM)
	defer signal.Stop(terms)
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-terms:
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
				fmt.Fprintf(c.stderr, "forward SIGTERM: %v\n", err) //nolint:errcheck // No other diagnostic stream is available.
			}
		case <-done:
		}
	}()
	err := cmd.Wait()
	close(done)
	<-finished
	return err
}

type app struct {
	runner         runner
	stdin          io.Reader
	stdout, stderr io.Writer
	interval       time.Duration
	home           string
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() { <-ctx.Done(); stop() }()
	a := app{runner: execRunner{}, stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, interval: time.Minute, home: os.Getenv("HOME")}
	code := a.run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}

func (a app) run(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("nix-switch", flag.ContinueOnError)
	flags.SetOutput(a.stderr)
	local := flags.Bool("local", false, "switch the current working copy without syncing")
	repo := flags.String("repo", "", "repository checkout; defaults to the config repo")
	configPath := flags.String("config", "", "CUE file with repo, flake, and hosts (required)")
	host := flags.String("host", "", "darwin configuration to switch to; must be listed in the config (required)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *configPath == "" {
		return a.usage("--config is required")
	}
	if *host == "" {
		return a.usage("--host is required")
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return a.fail("load config", err)
	}
	if !slices.Contains(cfg.Hosts, *host) {
		return a.usage(fmt.Sprintf("host %q is not listed in the config", *host))
	}
	repoPath := *repo
	if repoPath == "" {
		repoPath = configRepo(a.home, cfg.Repo)
	}
	absRepo, err := filepath.Abs(repoPath)
	if err != nil {
		return a.fail("resolve repo path", err)
	}
	base := command{stdin: a.stdin, stdout: a.stdout, stderr: a.stderr}
	sudo := base
	sudo.name, sudo.args = "sudo", []string{"-v"}
	if err := a.runner.run(ctx, sudo); err != nil {
		return a.fail("sudo -v", err)
	}
	keepCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go a.keepalive(keepCtx, done)
	defer func() { cancel(); <-done }()

	if *local {
		if code := a.describeLocal(ctx, absRepo); code != 0 {
			return code
		}
	} else if code := a.syncMain(ctx, absRepo, base); code != 0 {
		return code
	}
	return a.switchDarwin(ctx, absRepo, cfg.Flake, *host, flags.Args(), base)
}

// usage reports a bad invocation. The flag set has already printed flag errors.
func (a app) usage(msg string) int {
	fmt.Fprintln(a.stderr, msg) //nolint:errcheck // stderr is the only diagnostic stream; a failed write has nowhere to go.
	return 2
}

// configRepo resolves the config's repo path. A relative path is under $HOME.
func configRepo(home, repo string) string {
	if filepath.IsAbs(repo) {
		return repo
	}
	return filepath.Join(home, repo)
}

func (a app) describeLocal(ctx context.Context, repo string) int {
	current, err := a.query(ctx, repo, "change_id.short() ++ \" \" ++ description.first_line()")
	if err != nil {
		return a.fail("jj working-copy description", err)
	}
	if _, err := fmt.Fprintf(a.stdout, "Switching current working copy %s without syncing.\n", strings.TrimSpace(current)); err != nil {
		return a.fail("writing working-copy notice", err)
	}
	return 0
}

func (a app) syncMain(ctx context.Context, repo string, jj command) int {
	jj.name, jj.dir = "jj", repo
	if code := a.checkWorkingCopy(ctx, repo); code != 0 {
		return code
	}
	if _, err := fmt.Fprintln(a.stdout, "==> jj git fetch"); err != nil {
		return a.fail("writing fetch header", err)
	}
	jj.args = []string{"git", "fetch"}
	if err := a.runner.run(ctx, jj); err != nil {
		return a.fail("jj git fetch", err)
	}
	if code := a.checkMain(ctx, repo); code != 0 {
		return code
	}
	if _, err := fmt.Fprintln(a.stdout, "==> jj new main"); err != nil {
		return a.fail("writing new change header", err)
	}
	jj.args = []string{"new", "main"}
	if err := a.runner.run(ctx, jj); err != nil {
		return a.fail("jj new main", err)
	}
	return 0
}

func (a app) checkWorkingCopy(ctx context.Context, repo string) int {
	empty, err := a.query(ctx, repo, "empty")
	if err != nil {
		return a.fail("jj working-copy status", err)
	}
	if strings.TrimSpace(empty) != "false" {
		return 0
	}
	id, err := a.query(ctx, repo, "change_id.short()")
	if err != nil {
		return a.fail("jj working-copy change id", err)
	}
	return a.fail("working copy "+strings.TrimSpace(id)+" has uncommitted changes; run `nix-switch --local` to switch them, or commit/park them first", nil)
}

func (a app) checkMain(ctx context.Context, repo string) int {
	var out bytes.Buffer
	err := a.runner.run(ctx, command{name: "jj", args: []string{"bookmark", "list", "--conflicted", "main", "-T", "name"}, dir: repo, stdout: &out, stderr: a.stderr})
	if err != nil {
		return a.fail("jj main status", err)
	}
	if strings.TrimSpace(out.String()) != "" {
		return a.fail("main is conflicted", nil)
	}
	return 0
}

func (a app) switchDarwin(ctx context.Context, repo, flake, host string, args []string, nh command) int {
	if _, err := fmt.Fprintln(a.stdout, "==> nh darwin switch"); err != nil {
		return a.fail("writing switch header", err)
	}
	nh.name = "nh"
	nh.args = append([]string{"darwin", "switch", filepath.Join(repo, flake) + "#" + host, "--show-trace"}, args...)
	nh.env = []string{"NH_SHOW_ACTIVATION_LOGS=1"}
	if err := a.runner.run(ctx, nh); err != nil {
		return a.fail("nh darwin switch", err)
	}
	return 0
}

func (a app) query(ctx context.Context, repo, template string) (string, error) {
	var out bytes.Buffer
	err := a.runner.run(ctx, command{name: "jj", args: []string{"log", "-r", "@", "--no-graph", "-T", template}, dir: repo, stdout: &out, stderr: a.stderr})
	return out.String(), err
}

func (a app) keepalive(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			err := a.runner.run(ctx, command{name: "sudo", args: []string{"-n", "-v"}, stderr: a.stderr})
			if err != nil {
				return
			}
		}
	}
}

func (a app) fail(step string, err error) int {
	// There is no alternative diagnostic stream if stderr fails.
	if err == nil {
		fmt.Fprintln(a.stderr, step) //nolint:errcheck // There is no alternative diagnostic stream.
		return 1
	}
	_, _ = fmt.Fprintf(a.stderr, "%s: %v\n", step, err)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() > 0 {
		return exitErr.ExitCode()
	}
	return 1
}
