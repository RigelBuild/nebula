// Package main runs one CD target: it gates on the pipeline history, holds the
// Kubernetes Lease while the deploy runs, and exits with the deploy's code. The
// usage is in the usage constant below.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec" //nolint:depguard // run-cd invokes git and the deploy command directly, without a shell.
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/cdgate"
	"github.com/RigelBuild/nebula/tools/go-lib/cdrelease"
	"github.com/RigelBuild/nebula/tools/go-lib/stepoutput"
	"github.com/RigelBuild/nebula/tools/go-lib/woodpecker"
)

const usage = "usage: run-cd <target> [--lease <name>] --members <workflow,...> --in-cluster-url <url>" +
	" --pulumi-root <dir> --pulumi-tiers <tier,...> -- <command...>\n       run-cd --heartbeat"

// queueStore opens the release queue store. Tests replace it with a fake.
var queueStore = func(getenv func(string) string) (cdrelease.Store, error) {
	return cdrelease.NewConfigMapStore(getenv, os.ReadFile, nil)
}

func main() {
	os.Exit(runMain())
}

func runMain() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	return runSignalContext(ctx, stop, os.Args[1:], os.Getenv, os.Stderr)
}

func runSignalContext(ctx context.Context, stop func(), args []string, getenv func(string) string, stderr io.Writer) int {
	defer stop()
	return run(ctx, args, getenv, stderr)
}

// options is the parsed command line.
type options struct {
	target  string
	lease   string
	members []string
	command []string
	// inClusterURL is the Woodpecker API address reachable from inside the
	// cluster. The deployment supplies it; there is no default.
	inClusterURL string
	// pulumiRoot and pulumiTiers locate the Pulumi projects: each tier is a
	// directory under pulumiRoot whose children with a Pulumi.yaml are stacks.
	pulumiRoot  string
	pulumiTiers []string
}

// parseArgs reads the CLI. Flags must come before "--". A missing target,
// members list, in-cluster URL, Pulumi root, Pulumi tiers, or command is a
// usage error.
func parseArgs(args []string) (options, error) {
	target, args, ok := takeFirst(args)
	if !ok || target == "" {
		return options{}, errors.New(usage)
	}
	opts := options{target: target}
	for len(args) > 0 {
		arg, rest, ok := takeFirst(args)
		if !ok {
			return options{}, errors.New(usage)
		}
		args = rest
		if arg == "--" {
			opts.command = args
			break
		}
		switch arg {
		case "--lease":
			lease, rest, ok := takeFirst(args)
			if !ok || lease == "" {
				return options{}, errors.New(usage)
			}
			opts.lease = lease
			args = rest
		case "--members":
			members, rest, ok := takeFirst(args)
			if !ok {
				return options{}, errors.New(usage)
			}
			opts.members = nonEmpty(strings.Split(members, ","))
			args = rest
		case "--in-cluster-url", "--pulumi-root", "--pulumi-tiers":
			value, rest, ok := takeFirst(args)
			if !ok {
				return options{}, errors.New(usage)
			}
			setValueFlag(&opts, arg, value)
			args = rest
		default:
			return options{}, errors.New(usage)
		}
	}
	if len(opts.members) == 0 || len(opts.command) == 0 || opts.inClusterURL == "" ||
		opts.pulumiRoot == "" || len(opts.pulumiTiers) == 0 {
		return options{}, errors.New(usage)
	}
	return opts, nil
}

// setValueFlag stores the value of one of the string-valued deployment flags.
func setValueFlag(opts *options, flag, value string) {
	switch flag {
	case "--in-cluster-url":
		opts.inClusterURL = strings.TrimSpace(value)
	case "--pulumi-root":
		opts.pulumiRoot = strings.TrimSpace(value)
	case "--pulumi-tiers":
		opts.pulumiTiers = nonEmpty(strings.Split(value, ","))
	}
}

func takeFirst(parts []string) (string, []string, bool) {
	for i, part := range parts {
		return part, parts[i+1:], true
	}
	return "", nil, false
}

func nonEmpty(parts []string) []string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// run parses the CLI, runs the target, and returns the exit code. A usage error
// exits 2. A stop from the signal context turns a clean run or a skip into exit
// 1, so a cancelled step never reports success.
func run(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "--heartbeat" {
		code := cdgate.RunHeartbeat(ctx, releaseFence(getenv), stderr)
		if ctx.Err() != nil && code == 0 {
			return 1
		}
		return code
	}
	opts, err := parseArgs(args)
	if err != nil {
		if _, writeErr := fmt.Fprintln(stderr, err); writeErr != nil {
			return 1
		}
		return 2
	}
	return stopExit(ctx, runCD(ctx, opts, getenv, stderr))
}

// stopExit fails a stopped run that would otherwise read as success or skip.
func stopExit(ctx context.Context, code int) int {
	if ctx.Err() != nil && (code == 0 || code == cdgate.CDSkipExitCode) {
		return 1
	}
	return code
}

// runCD wires the environment and the OS seams into cdgate.RunCD. The gate reads
// the pipeline history; the lease is set up from the cluster environment only
// when --lease is given.
func runCD(ctx context.Context, opts options, getenv func(string) string, stderr io.Writer) int {
	r, err := newRun(opts, getenv, stderr)
	if err != nil {
		if _, writeErr := fmt.Fprintf(stderr, "run-cd: %v\n", err); writeErr != nil {
			return 1
		}
		return 1
	}
	return cdgate.RunCD(ctx, opts.target, r)
}

func newRun(opts options, getenv func(string) string, stderr io.Writer) (cdgate.Run, error) {
	pulumiStacks, err := cdgate.ReadPulumiStacks(os.DirFS("."), opts.pulumiRoot, opts.pulumiTiers)
	if err != nil {
		return cdgate.Run{}, fmt.Errorf("reading Pulumi stacks: %w", err)
	}
	// The decision is read lazily inside the Gate. An invalid CD API context,
	// such as a missing or rejected token, blocks the deploy; other gate errors
	// fail open and deploy.
	var (
		setupMu  sync.Mutex
		ready    bool
		decision cdgate.Decision
		own      cdgate.Own
	)
	setup := func(ctx context.Context) error {
		pipelines, err := cdgate.PipelineAPIFromEnv(ctx, getenv, opts.inClusterURL, http.DefaultClient)
		if err != nil {
			return fmt.Errorf("resolving CD API context: %w", err)
		}
		resolved, err := cdgate.OwnFromEnv(getenv, opts.target)
		if err != nil {
			return err
		}
		own = resolved
		decision = cdgate.Decision{
			Pipelines: pipelines,
			Ranks: func(ctx context.Context) (string, map[string]int, error) {
				ranks, err := cdgate.MainRanks(ctx, gitRunner)
				if err != nil {
					return "", nil, err
				}
				return headCommit(ranks), ranks, nil
			},
			Members:      opts.members,
			PulumiStacks: pulumiStacks,
		}
		return nil
	}
	r := cdgate.Run{
		Gate: func(ctx context.Context) (cdgate.Verdict, error) {
			setupMu.Lock()
			defer setupMu.Unlock()
			if !ready {
				if err := setup(ctx); err != nil {
					return cdgate.Verdict{}, err
				}
				ready = true
			}
			return decision.Read(ctx, own)
		},
		Skip: func(verdict cdgate.Verdict) {
			stepoutput.Write(getenv, stderr, "deploy.state", "skipped")
			stepoutput.Write(getenv, stderr, "deploy.reason", verdict.Reason)
		},
		Exec:         execDeploy(opts.command, deployEnv()),
		PulumiStacks: pulumiStacks,
		Clock:        cdgate.Clock{Wall: time.Now, Monotonic: monotonicSince(time.Now())},
		Sleep:        sleepFor,
		Log:          stderr,
	}
	if opts.lease != "" {
		leaseEnv := func(name string) string {
			if name == "CD_LEASE_NAME" {
				return opts.lease
			}
			return getenv(name)
		}
		r.PrepareLease = func() (*cdgate.LeaseConfig, error) {
			return cdgate.CreateKubernetesLeaseClient(leaseEnv, os.ReadFile, nil)
		}
		r.Fence = releaseFence(getenv)
	}
	if opts.lease == "" && getenv(cdgate.DispatchIDVariable) != "" {
		r.PublishFence = newestReleaseFence(getenv, opts.inClusterURL)
	}
	return r, nil
}

// releaseFence fences a Lease-taking deploy step through the release queue. It
// runs under the Lease, so the store read and the heartbeat are one write.
func releaseFence(getenv func(string) string) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		pipeline, err := cdgate.ParsePositiveInt(strings.TrimSpace(getenv("CI_PIPELINE_NUMBER")), "CI_PIPELINE_NUMBER")
		if err != nil {
			return false, err
		}
		store, err := queueStore(getenv)
		if err != nil {
			return false, err
		}
		return cdgate.ReleaseFence(store, pipeline, getenv(cdgate.DispatchIDVariable), time.Now)(ctx)
	}
}

// newestReleaseFence fences a publish step, which takes no Lease, through the
// Woodpecker API: its pipeline must be the newest release.
func newestReleaseFence(getenv func(string) string, inClusterURL string) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		own, err := cdgate.ParsePositiveInt(strings.TrimSpace(getenv("CI_PIPELINE_NUMBER")), "CI_PIPELINE_NUMBER")
		if err != nil {
			return false, err
		}
		pipelines, err := cdgate.PipelineAPIFromEnv(ctx, getenv, inClusterURL, http.DefaultClient)
		if err != nil {
			return false, fmt.Errorf("resolving CD API context: %w", err)
		}
		return pipelines.IsNewestRelease(ctx, own)
	}
}

// gitRunner runs git in the working directory, returning stdout. Each call is
// bounded by the gate's read timeout, so a hung git fails the read and the gate
// fails open instead of blocking the target. The caller's ctx still cancels it.
func gitRunner(ctx context.Context, args ...string) (string, error) {
	return runGit(ctx, woodpecker.GateTimeout, args...)
}

// runGit runs git with a read deadline. A git that outlives the deadline is
// killed and returns an error, which the caller treats as a gate error.
func runGit(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // The command is the fixed git executable; args come from the internal gate.
	// git spawns helpers that inherit stdout. Kill the whole group on the
	// deadline, and cap how long a leftover pipe can hold Output open, so a
	// hung helper cannot outlive the bound.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	return string(out), err
}

// headCommit returns the rank-0 commit of main's history, the head.
func headCommit(ranks map[string]int) string {
	for commit, rank := range ranks {
		if rank == 0 {
			return commit
		}
	}
	return ""
}

// execDeploy runs the deploy command through the process-group executor. Its
// output is captured only when the caller retries, which is decided per target.
func execDeploy(command []string, env []string) cdgate.Executor {
	return func(ctx context.Context, capture bool) (cdgate.StepResult, error) {
		var oc *cdgate.OrderedCapture
		if capture {
			oc = cdgate.NewOrderedCapture()
		}
		out, err := cdgate.ExecGroup(ctx,
			cdgate.OSRunner{Stdin: os.Stdin, Env: env},
			cdgate.OSGroupProbe{}, groupClock(), command, os.Stdout, os.Stderr, oc)
		if err != nil {
			return cdgate.StepResult{}, err
		}
		return cdgate.StepResult(out), nil
	}
}

// groupClock is the production time seam for the process-group executor.
func groupClock() cdgate.GroupClock {
	return cdgate.GroupClock{
		Now:   time.Now,
		Sleep: sleepFor,
		After: time.After,
	}
}

// sleepFor waits d or until ctx is done, returning ctx.Err() when cancelled.
func sleepFor(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// monotonicSince returns a Monotonic clock that counts from start. It uses
// time.Since, which reads the monotonic reading embedded in start.
func monotonicSince(start time.Time) func() time.Duration {
	return func() time.Duration { return time.Since(start) }
}

// deployEnv is the deploy's environment: everything except the CD API token,
// which the wrapper holds and the deploy must not see.
func deployEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "WOODPECKER_CD_TOKEN=") {
			continue
		}
		env = append(env, kv)
	}
	return env
}
