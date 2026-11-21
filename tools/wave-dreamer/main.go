package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec" //nolint:depguard // The runner invokes the configured local tools.
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

// Required environment: the dreamer clone path, its git remote, and the done-file directory.
const (
	envClone   = "WAVE_DREAMER_CLONE"
	envRemote  = "WAVE_DREAMER_REMOTE"
	envRunsDir = "WAVE_DREAMER_RUNS_DIR"
)

var (
	dayPattern     = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	sessionPattern = regexp.MustCompile(`^dreamer-[0-9-]+$`)
	journalPattern = regexp.MustCompile(`(?m)^  autolearn\.journalDir = `)
)

type outputMode int

const (
	inheritOutput outputMode = iota
	captureOutput
	discardOutput
)

type command struct {
	dir    string
	name   string
	args   []string
	env    []string
	stdout outputMode
	stderr outputMode
}

type executeFunc func(context.Context, command) ([]byte, error)

type fileSystem interface {
	Stat(path string) (os.FileInfo, error)
	Glob(pattern string) ([]string, error)
	MkdirAll(path string, mode os.FileMode) error
	Rename(oldPath, newPath string) error
}

type dependencies struct {
	execute executeFunc
	fs      fileSystem
	getenv  func(string) string
	clone   string
	remote  string
	runsDir string
}

type osFileSystem struct{}

func (osFileSystem) Stat(path string) (os.FileInfo, error) { return os.Stat(path) }
func (osFileSystem) Glob(pattern string) ([]string, error) { return filepath.Glob(pattern) }
func (osFileSystem) MkdirAll(path string, mode os.FileMode) error {
	return os.MkdirAll(path, mode)
}
func (osFileSystem) Rename(oldPath, newPath string) error { return os.Rename(oldPath, newPath) }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	root := newRootCommand(ctx, dependencies{execute: execute, fs: osFileSystem{}, getenv: os.Getenv})
	root.SetErr(os.Stderr)
	if err := root.Execute(); err != nil {
		if _, writeErr := fmt.Fprintf(os.Stderr, "wave-dreamer: %v\n", err); writeErr != nil {
			stop()
			os.Exit(1)
		}
		stop()
		os.Exit(1)
	}
	stop()
}

func run(ctx context.Context, deps dependencies) error {
	for _, name := range []string{envClone, envRemote, envRunsDir} {
		if deps.getenv(name) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	deps.clone = deps.getenv(envClone)
	deps.remote = deps.getenv(envRemote)
	deps.runsDir = deps.getenv(envRunsDir)
	day, err := deps.date(ctx, "+%F")
	if err != nil {
		return fmt.Errorf("read UTC day: %w", err)
	}
	if exists, err := deps.exists(filepath.Join(deps.runsDir, day+".done")); err != nil {
		return fmt.Errorf("check done file: %w", err)
	} else if exists {
		return nil
	}
	if err := deps.checkJournalMode(ctx); err != nil {
		return err
	}
	if err := deps.prepareClone(ctx); err != nil {
		return err
	}
	return deps.startDream(ctx, day)
}

func (deps dependencies) checkJournalMode(ctx context.Context) error {
	configOutput, err := deps.omp(ctx, "", []string{"--config", deps.getenv("WAVE_DREAMER_CONFIG"), "config", "list"}, true)
	if err != nil {
		return errors.New("omp config list failed; no .done written")
	}
	if !journalPattern.Match(configOutput) {
		return errors.New("installed omp has no autolearn.journalDir setting (T1 missing); refusing to run; no .done written")
	}
	return nil
}

func (deps dependencies) prepareClone(ctx context.Context) error {
	if err := deps.ensureClone(ctx); err != nil {
		return err
	}
	jj := deps.commandPath("WAVE_DREAMER_JJ", "jj")
	if err := deps.runCommand(ctx, deps.clone, jj, "git", "fetch"); err != nil {
		return fmt.Errorf("jj git fetch: %w", err)
	}
	if err := deps.runCommand(ctx, deps.clone, jj, "new", "main@origin"); err != nil {
		return fmt.Errorf("jj new main@origin: %w", err)
	}
	return nil
}

func (deps dependencies) startDream(ctx context.Context, day string) error {
	sessionTime, err := deps.date(ctx, "+%H%M%S")
	if err != nil {
		return fmt.Errorf("read UTC session time: %w", err)
	}
	session := "dreamer-" + day + "-" + sessionTime
	if !dayPattern.MatchString(day) || !sessionPattern.MatchString(session) {
		return errors.New("day or session id has an unexpected shape")
	}
	maxTime := deps.getenv("WAVE_DREAMER_MAX_TIME")
	if maxTime == "" {
		maxTime = "30m"
	}
	args := []string{
		"-p", "--max-time", maxTime, "--session-id", session,
		"--model", "@dream", "--tools", "read,bash,manage_skill",
		"--config", deps.getenv("WAVE_DREAMER_CONFIG"), "@" + deps.getenv("WAVE_DREAMER_PROMPT"),
		"Run " + session + " for day " + day + ". Write the done file as " + day + ".done.",
	}
	if _, err := deps.omp(ctx, deps.clone, args, false); err != nil {
		return fmt.Errorf("omp dreamer run: %w", err)
	}
	return nil
}

func (deps dependencies) ensureClone(ctx context.Context) error {
	jj := deps.commandPath("WAVE_DREAMER_JJ", "jj")
	if deps.cloneIsValid(ctx, jj) {
		return nil
	}
	if exists, err := deps.exists(deps.clone); err != nil {
		return fmt.Errorf("check dreamer clone: %w", err)
	} else if exists {
		brokenClones, err := deps.fs.Glob(deps.clone + ".broken-*")
		if err != nil {
			return fmt.Errorf("list moved-aside clones: %w", err)
		}
		if len(brokenClones) > 0 {
			return fmt.Errorf("%s already exists; fix or remove it before the next run; no clone made", brokenClones[0])
		}
		stamp, err := deps.date(ctx, "+%Y%m%dT%H%M%SZ")
		if err != nil {
			return fmt.Errorf("read UTC broken-clone time: %w", err)
		}
		if err := deps.fs.Rename(deps.clone, deps.clone+".broken-"+stamp); err != nil {
			return fmt.Errorf("move broken dreamer clone aside: %w", err)
		}
	}
	if err := deps.fs.MkdirAll(filepath.Dir(deps.clone), 0o700); err != nil {
		return fmt.Errorf("create dreamer clone parent: %w", err)
	}
	if err := deps.runCommand(ctx, "", jj, "git", "clone", "--colocate", deps.remote, deps.clone); err != nil {
		return fmt.Errorf("clone dreamer repository: %w", err)
	}
	return nil
}

func (deps dependencies) cloneIsValid(ctx context.Context, jj string) bool {
	remotes, err := deps.execute(ctx, command{
		dir:    deps.clone,
		name:   jj,
		args:   []string{"-R", deps.clone, "--ignore-working-copy", "git", "remote", "list"},
		stdout: captureOutput,
		stderr: discardOutput,
	})
	if err != nil || !hasRemote(string(remotes), deps.remote) {
		return false
	}
	_, err = deps.execute(ctx, command{
		dir:    deps.clone,
		name:   jj,
		args:   []string{"-R", deps.clone, "--ignore-working-copy", "log", "-r", "main@origin", "--no-graph", "-T", "commit_id"},
		stdout: discardOutput,
		stderr: discardOutput,
	})
	return err == nil
}

func (deps dependencies) date(ctx context.Context, format string) (string, error) {
	output, err := deps.execute(ctx, command{
		name:   deps.commandPath("WAVE_DREAMER_DATE", "date"),
		args:   []string{"-u", format},
		stdout: captureOutput,
		stderr: inheritOutput,
	})
	return strings.TrimSpace(string(output)), err
}

func (deps dependencies) runCommand(ctx context.Context, dir, name string, args ...string) error {
	_, err := deps.execute(ctx, command{
		dir:    dir,
		name:   name,
		args:   args,
		stdout: inheritOutput,
		stderr: inheritOutput,
	})
	return err
}

func (deps dependencies) exists(path string) (bool, error) {
	_, err := deps.fs.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (deps dependencies) commandPath(key, fallback string) string {
	if path := deps.getenv(key); path != "" {
		return path
	}
	return fallback
}

func (deps dependencies) omp(ctx context.Context, dir string, args []string, captureStdout bool) ([]byte, error) {
	bridgeOutput, err := deps.execute(ctx, command{
		name:   deps.getenv("WAVE_DREAMER_BRIDGE"),
		args:   []string{"--omp-env"},
		stdout: captureOutput,
		stderr: inheritOutput,
	})
	if err != nil {
		return nil, fmt.Errorf("load OMP environment from wave bridge: %w", err)
	}
	env, err := deps.ompEnvironment(bridgeOutput)
	if err != nil {
		return nil, err
	}
	stdout := inheritOutput
	if captureStdout {
		stdout = captureOutput
	}
	return deps.execute(ctx, command{
		dir:    dir,
		name:   deps.commandPath("WAVE_DREAMER_OMP", "omp"),
		args:   args,
		env:    env,
		stdout: stdout,
		stderr: inheritOutput,
	})
}

func (deps dependencies) ompEnvironment(bridgeOutput []byte) ([]string, error) {
	values, err := parseBridgeEnvironment(bridgeOutput)
	if err != nil {
		return nil, err
	}
	env := []string{"PATH=" + deps.getenv("WAVE_DREAMER_OMP_PATH"), "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0="}
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		deps.appendEnv(&env, key)
	}
	for _, key := range []string{"LITELLM_BASE_URL", "LITELLM_API_KEY", "SSL_CERT_FILE"} {
		if value, ok := values[key]; ok {
			env = append(env, key+"="+value)
		}
	}
	deps.appendEnv(&env, "HINDSIGHT_URL")
	return env, nil
}

func parseBridgeEnvironment(output []byte) (map[string]string, error) {
	allowed := map[string]bool{"LITELLM_BASE_URL": true, "LITELLM_API_KEY": true, "SSL_CERT_FILE": true}
	values := map[string]string{}
	for entry := range bytes.SplitSeq(output, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		key, value, ok := strings.Cut(string(entry), "=")
		if !ok || !allowed[key] {
			return nil, fmt.Errorf("wave bridge returned an unexpected OMP environment entry %q", key)
		}
		if _, duplicate := values[key]; duplicate {
			return nil, fmt.Errorf("wave bridge returned duplicate OMP environment entry %q", key)
		}
		values[key] = value
	}
	return values, nil
}

func (deps dependencies) appendEnv(env *[]string, key string) {
	if value := deps.getenv(key); value != "" {
		*env = append(*env, key+"="+value)
	}
}

func hasRemote(output, remote string) bool {
	for line := range strings.SplitSeq(output, "\n") {
		if line == "origin "+remote {
			return true
		}
	}
	return false
}

func execute(ctx context.Context, request command) ([]byte, error) {
	cmd := exec.CommandContext(ctx, request.name, request.args...) //nolint:gosec // Commands and arguments come from the service configuration.
	cmd.Dir = request.dir
	cmd.Env = request.env
	var output bytes.Buffer
	switch request.stdout {
	case inheritOutput:
		cmd.Stdout = os.Stdout
	case captureOutput:
		cmd.Stdout = &output
	case discardOutput:
		cmd.Stdout = io.Discard
	}
	switch request.stderr {
	case inheritOutput:
		cmd.Stderr = os.Stderr
	case captureOutput:
		cmd.Stderr = &output
	case discardOutput:
		cmd.Stderr = io.Discard
	}
	if err := cmd.Run(); err != nil {
		return output.Bytes(), err
	}
	return output.Bytes(), nil
}

func newRootCommand(ctx context.Context, deps dependencies) *cobra.Command {
	return &cobra.Command{
		Use:           "wave-dreamer",
		Short:         "Run the daily wave corpus dreamer",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE:          func(_ *cobra.Command, _ []string) error { return run(ctx, deps) },
	}
}
