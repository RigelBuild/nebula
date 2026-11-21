// SPDX-License-Identifier: MIT OR Apache-2.0

// omp-pool stages OMP OAuth credentials, uploads them, removes broker credentials, and resets broker usage state.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	defaultStageDir   = ".local/share/omp-pool"
	providerDefault   = "anthropic"
	poolPrimary       = "primary"
	commandOMP        = "omp"
	commandSSH        = "ssh"
	flagDryRun        = "--dry-run"
	flagYes           = "--yes"
	flagHost          = "--host"
	hostRequiresValue = "--host requires a value"
)

type usageError struct{ message string }

func (e usageError) Error() string { return e.message }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	return runWithRunner(ctx, args, getenv, os.Stdin, stdout, stderr, execRunner{})
}

func runWithRunner(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer, runner commandRunner) int {
	if len(args) == 0 {
		return noCommand(stderr)
	}
	return reportCommandResult(dispatchCommand(ctx, args, getenv, stdin, stdout, stderr, runner), stderr)
}

func reportCommandResult(result commandResult, stderr io.Writer) int {
	if result.err == nil {
		return result.code
	}
	return reportRunError(stderr, result.code, result.err)
}

type commandResult struct {
	code int
	err  error
}

func noCommand(stderr io.Writer) int {
	return reportUsageError(stderr, nil)
}

func dispatchCommand(
	ctx context.Context,
	args []string,
	getenv func(string) string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	runner commandRunner,
) commandResult {
	switch args[0] {
	case "login":
		return commandResultFrom(runLogin(ctx, args[1:], getenv, stdin, stdout, stderr, runner))
	case "push":
		return commandResultFrom(runPush(ctx, args[1:], getenv, stdin, stdout, stderr, runner))
	case "remove":
		return commandResultFrom(runRemove(ctx, args[1:], getenv, stdin, stdout, runner))
	case "reset-usage":
		return commandResultFrom(runResetUsage(ctx, args[1:], getenv, stdin, stdout, runner))
	case "status":
		return commandResultFrom(runStatus(args[1:], getenv, stdout))
	default:
		return unknownCommand(args[0])
	}
}

func commandResultFrom(code int, err error) commandResult {
	return commandResult{code: code, err: err}
}

func unknownCommand(command string) commandResult {
	return commandResult{code: 2, err: usageError{message: fmt.Sprintf("unknown command %q", command)}}
}

func reportRunError(stderr io.Writer, code int, err error) int {
	if _, ok := errors.AsType[usageError](err); ok {
		return reportUsageError(stderr, err)
	}
	if _, writeErr := fmt.Fprintf(stderr, "omp-pool: %v\n", err); writeErr != nil {
		return 1
	}
	if code == 0 {
		return 1
	}
	return code
}

func reportUsageError(stderr io.Writer, err error) int {
	if writeErr := writeUsage(stderr); writeErr != nil {
		return 1
	}
	if err == nil {
		return 2
	}
	if _, writeErr := fmt.Fprintf(stderr, "omp-pool: %v\n", err); writeErr != nil {
		return 1
	}
	return 2
}

func writeUsage(w io.Writer) error {
	_, err := io.WriteString(w, "usage: omp-pool login <primary> [provider]\n       omp-pool push <primary> [--dry-run] [--yes] [--host H]\n       omp-pool remove <primary> <account> [--dry-run] [--yes] [--host H]\n       omp-pool reset-usage <primary> <account> [--dry-run] [--yes] [--host H]\n       omp-pool status\n")
	if err != nil {
		return fmt.Errorf("write usage: %w", err)
	}
	return nil
}

func parseLoginArgs(args []string) (pool, provider string, err error) {
	if len(args) < 1 || len(args) > 2 {
		return "", "", usageError{message: "login requires a pool and optional provider"}
	}
	pool = args[0]
	if !validPool(pool) {
		return "", "", usageError{message: fmt.Sprintf("invalid pool %q", pool)}
	}
	provider = providerDefault
	if len(args) == 2 {
		provider = args[1]
		if provider == "" {
			return "", "", usageError{message: "provider must not be empty"}
		}
	}
	return pool, provider, nil
}

type pushOptions struct {
	pool         string
	dryRun       bool
	yes          bool
	hostOverride string
	deployment
}

func parsePushArgs(args []string, getenv func(string) string) (pushOptions, error) {
	options, err := initialPushOptions(args)
	if err != nil {
		return pushOptions{}, err
	}
	if err := parsePushOptions(args[1:], &options); err != nil {
		return pushOptions{}, err
	}
	d, err := loadDeployment(getenv, options.hostOverride)
	if err != nil {
		return pushOptions{}, err
	}
	options.deployment = d
	return options, nil
}

func initialPushOptions(args []string) (pushOptions, error) {
	if len(args) == 0 {
		return pushOptions{}, usageError{message: "push requires a pool"}
	}
	options := pushOptions{pool: args[0]}
	if !validPool(options.pool) {
		return pushOptions{}, usageError{message: fmt.Sprintf("invalid pool %q", options.pool)}
	}
	return options, nil
}

func parsePushOptions(args []string, options *pushOptions) error {
	for len(args) > 0 {
		consumed, err := parsePushOption(args, options)
		if err != nil {
			return err
		}
		args = args[consumed:]
	}
	return nil
}

func parsePushOption(args []string, options *pushOptions) (int, error) {
	switch args[0] {
	case flagDryRun:
		options.dryRun = true
	case flagYes:
		options.yes = true
	case flagHost:
		if len(args) < 2 || args[1] == "" {
			return 0, usageError{message: hostRequiresValue}
		}
		options.hostOverride = args[1]
		return 2, nil
	default:
		return 0, usageError{message: fmt.Sprintf("unknown push option %q", args[0])}
	}
	return 1, nil
}

func validPool(pool string) bool {
	return pool == poolPrimary
}

// Environment names for the deployment inputs. None has a default: a missing
// value fails the command with usage exit code 2.
const (
	envHost         = "OMP_POOL_HOST"
	envKubeconfig   = "OMP_POOL_KUBECONFIG"
	envNamespace    = "OMP_POOL_NAMESPACE"
	envBrokerPod    = "OMP_POOL_BROKER_POD"
	envBrokerSecret = "OMP_POOL_BROKER_SECRET"
)

// deployment is the cluster and SSH target for the broker. Every field is
// required; the command reads them from the environment, and --host may
// override host.
type deployment struct {
	host       string
	kubeconfig string
	namespace  string
	pod        string
	secret     string
}

// loadDeployment reads the required deployment inputs. A non-empty override
// replaces the host from OMP_POOL_HOST. The error names every missing input.
func loadDeployment(getenv func(string) string, hostOverride string) (deployment, error) {
	d := deployment{
		host:       hostOverride,
		kubeconfig: getenv(envKubeconfig),
		namespace:  getenv(envNamespace),
		pod:        getenv(envBrokerPod),
		secret:     getenv(envBrokerSecret),
	}
	if d.host == "" {
		d.host = getenv(envHost)
	}
	var missing []string
	for _, required := range []struct{ name, value string }{
		{envHost, d.host},
		{envKubeconfig, d.kubeconfig},
		{envNamespace, d.namespace},
		{envBrokerPod, d.pod},
		{envBrokerSecret, d.secret},
	} {
		if required.value == "" {
			missing = append(missing, required.name)
		}
	}
	if len(missing) > 0 {
		return deployment{}, usageError{message: "missing required environment variable(s): " + strings.Join(missing, ", ")}
	}
	return d, nil
}

func stageRoot(getenv func(string) string) (string, error) {
	if dir := getenv("OMP_POOL_STAGE_DIR"); dir != "" {
		return dir, nil
	}
	home := getenv("HOME")
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find home directory: %w", err)
		}
	}
	return filepath.Join(home, defaultStageDir), nil
}

func poolStageDir(root, pool string) string {
	return filepath.Join(root, pool)
}

func runLogin(
	ctx context.Context,
	args []string,
	getenv func(string) string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	runner commandRunner,
) (int, error) {
	pool, provider, err := parseLoginArgs(args)
	if err != nil {
		return 2, err
	}
	root, err := stageRoot(getenv)
	if err != nil {
		return 1, err
	}
	stageDir := poolStageDir(root, pool)
	if err := os.MkdirAll(stageDir, 0o700); err != nil {
		return 1, fmt.Errorf("create staging directory: %w", err)
	}
	// 0700 is required for the private staging directory; G302's file mode check does not apply to directories.
	info, err := os.Stat(stageDir)
	if err != nil {
		return 1, fmt.Errorf("check staging directory: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return 1, errors.New("staging directory must not grant group or world access")
	}
	childEnv := loginEnv(os.Environ(), stageDir)
	if err := runner.RunOMP(ctx, loginArgv(provider), childEnv, stdin, stdout, stderr); err != nil {
		return 1, fmt.Errorf("run OMP login: %w", err)
	}
	return 0, nil
}

func loginArgv(provider string) []string {
	return []string{"auth-broker", "login", provider}
}

func loginEnv(base []string, stageDir string) []string {
	return envWith(base, stagedAgentEnv(stageDir), brokerAuthEnv...)
}

func runStatus(args []string, getenv func(string) string, stdout io.Writer) (int, error) {
	if len(args) != 0 {
		return 2, usageError{message: "status takes no arguments"}
	}
	root, err := stageRoot(getenv)
	if err != nil {
		return 1, err
	}
	db := filepath.Join(poolStageDir(root, poolPrimary), "agent.db")
	_, err = os.Stat(db)
	switch {
	case err == nil:
		if _, err := fmt.Fprintf(stdout, "%s: staged login exists\n", poolPrimary); err != nil {
			return 1, fmt.Errorf("write status: %w", err)
		}
	case errors.Is(err, os.ErrNotExist):
		if _, err := fmt.Fprintf(stdout, "%s: no staged login\n", poolPrimary); err != nil {
			return 1, fmt.Errorf("write status: %w", err)
		}
	case err != nil:
		return 1, fmt.Errorf("check %s staging database: %w", poolPrimary, err)
	}
	return 0, nil
}

func envWith(base []string, set map[string]string, remove ...string) []string {
	values := make(map[string]string, len(base)+len(set))
	for _, item := range base {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			values[key] = value
		}
	}
	for _, key := range remove {
		delete(values, key)
	}
	maps.Copy(values, set)
	result := make([]string, 0, len(values))
	for key, value := range values {
		result = append(result, key+"="+value)
	}
	return result
}

var brokerAuthEnv = []string{"OMP_AUTH_BROKER_URL", "OMP_AUTH_BROKER_TOKEN"}

// stagedAgentEnv pins OMP to the staged dir. A profile replaces PI_CODING_AGENT_DIR;
// "default" selects none, and OMP's dotenv merge only fills empty keys.
func stagedAgentEnv(stageDir string) map[string]string {
	return map[string]string{"PI_CODING_AGENT_DIR": stageDir, "OMP_PROFILE": "default", "PI_PROFILE": "default"}
}

func envWithoutBrokerAuth() []string {
	return envWith(os.Environ(), nil, brokerAuthEnv...)
}
