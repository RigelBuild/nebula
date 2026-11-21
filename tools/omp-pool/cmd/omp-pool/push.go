// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
)

const primaryPort = 4002

type credential struct {
	Source   string `json:"source"`
	Provider string `json:"provider"`
	Identity string `json:"identity"`
}

type skippedCredential struct {
	credential
	Reason string `json:"reason"`
}

type migrationPlan struct {
	DryRun  bool                `json:"dryRun"`
	Plan    []credential        `json:"plan"`
	Skipped []skippedCredential `json:"skipped"`
}

func parseMigrationPlan(data []byte) (migrationPlan, error) {
	var plan migrationPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return migrationPlan{}, fmt.Errorf("parse OMP migration plan: %w", err)
	}
	if plan.Plan == nil || plan.Skipped == nil {
		return migrationPlan{}, errors.New("OMP migration plan is missing plan or skipped entries")
	}
	return plan, nil
}

func validateTargetPlan(plan migrationPlan) error {
	for _, item := range plan.Plan {
		if err := validateCredential(item); err != nil {
			return err
		}
	}
	return nil
}

func validateCredential(item credential) error {
	if item.Identity == "(api key)" {
		return fmt.Errorf("refusing non-OAuth credential %s (%s)", item.Provider, item.Identity)
	}
	return nil
}

func migrateArgv(dryRun, jsonOutput bool) []string {
	args := []string{"auth-broker", "migrate", "--from-local", "--include-oauth"}
	if dryRun {
		args = append(args, "--dry-run")
	}
	if jsonOutput {
		args = append(args, "--json")
	}
	return args
}

func brokerURL(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

type pushStreams struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

func runPush(
	parent context.Context,
	args []string,
	getenv func(string) string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	runner commandRunner,
) (int, error) {
	options, err := parsePushArgs(args, getenv)
	if err != nil {
		return 2, err
	}
	return runConfiguredPush(parent, options, getenv, pushStreams{stdin: stdin, stdout: stdout, stderr: stderr}, runner)
}

func runConfiguredPush(parent context.Context, options pushOptions, getenv func(string) string, streams pushStreams, runner commandRunner) (int, error) {
	stageDir, err := pushStageDir(getenv, options.pool)
	if err != nil {
		return 1, err
	}
	if err := validateStagedDatabase(stageDir, options.pool); err != nil {
		return 1, err
	}
	return executePush(parent, options, stageDir, streams, runner)
}

func pushStageDir(getenv func(string) string, pool string) (string, error) {
	root, err := stageRoot(getenv)
	if err != nil {
		return "", err
	}
	return poolStageDir(root, pool), nil
}

func executePush(
	parent context.Context,
	options pushOptions,
	stageDir string,
	streams pushStreams,
	runner commandRunner,
) (code int, retErr error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	return executePreparedPush(ctx, options, stageDir, streams, runner)
}

func executePreparedPush(ctx context.Context, options pushOptions, stageDir string, streams pushStreams, runner commandRunner) (int, error) {
	forward, err := startBrokerForward(ctx, options.deployment, runner)
	if err != nil {
		return 1, err
	}
	code, runErr := executeWithForward(ctx, options, stageDir, streams, forward, runner)
	if closeErr := forward.Close(); closeErr != nil {
		return 1, errors.Join(runErr, fmt.Errorf("close SSH forward: %w", closeErr))
	}
	return code, runErr
}

func executeWithForward(ctx context.Context, options pushOptions, stageDir string, streams pushStreams, forward brokerForward, runner commandRunner) (int, error) {
	token, err := readBrokerToken(ctx, options.deployment, runner)
	if err != nil {
		return 1, err
	}
	plan, err := previewBrokerPlan(ctx, stageDir, forward, token, streams.stderr, runner)
	if err != nil {
		return 1, err
	}
	return completePush(ctx, options, stageDir, forward, token, plan, streams, runner)
}

func completePush(
	ctx context.Context,
	options pushOptions,
	stageDir string,
	forward brokerForward,
	token string,
	targetPlan migrationPlan,
	streams pushStreams,
	runner commandRunner,
) (int, error) {
	if err := validateTargetPlan(targetPlan); err != nil {
		return 1, err
	}
	return handlePushPlan(ctx, options, stageDir, forward, token, targetPlan, streams, runner)
}

func handlePushPlan(
	ctx context.Context,
	options pushOptions,
	stageDir string,
	forward brokerForward,
	token string,
	plan migrationPlan,
	streams pushStreams,
	runner commandRunner,
) (int, error) {
	if len(plan.Plan) == 0 {
		if _, err := io.WriteString(streams.stdout, "nothing to push\n"); err != nil {
			return 1, fmt.Errorf("write empty plan result: %w", err)
		}
		return 0, nil
	}
	if err := printPlan(streams.stdout, plan.Plan); err != nil {
		return 1, err
	}
	if options.dryRun {
		return 0, nil
	}
	return confirmAndUpload(ctx, options, stageDir, forward, token, plan, streams, runner)
}

func confirmAndUpload(
	ctx context.Context,
	options pushOptions,
	stageDir string,
	forward brokerForward,
	token string,
	plan migrationPlan,
	streams pushStreams,
	runner commandRunner,
) (int, error) {
	if !options.yes {
		confirmed, err := confirmPush(streams.stdin, streams.stdout, len(plan.Plan), options.pool)
		if err != nil {
			return 1, err
		}
		if !confirmed {
			return 0, nil
		}
	}
	if err := uploadCredentials(ctx, stageDir, forward, token, options.deployment, streams, runner); err != nil {
		return 1, err
	}
	return 0, nil
}

func validateStagedDatabase(stageDir, pool string) error {
	dbPath := filepath.Join(stageDir, "agent.db")
	info, err := os.Stat(dbPath)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no staged login for %s (missing agent.db)", pool)
	}
	if err != nil {
		return fmt.Errorf("check %s staging database: %w", pool, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("staged agent.db for %s is not a regular file", pool)
	}
	return nil
}

func startBrokerForward(ctx context.Context, d deployment, runner commandRunner) (brokerForward, error) {
	forward, err := runner.StartForward(ctx, d, primaryPort)
	if err != nil {
		return nil, fmt.Errorf("start SSH forward to %s k3s broker: %w", poolPrimary, err)
	}
	if err := forward.WaitReady(ctx); err != nil {
		closeErr := forward.Close()
		if closeErr != nil {
			closeErr = fmt.Errorf("close SSH forward: %w", closeErr)
		}
		return nil, errors.Join(fmt.Errorf("wait for %s k3s broker forward: %w", poolPrimary, err), closeErr)
	}
	return forward, nil
}

func readBrokerToken(ctx context.Context, d deployment, runner commandRunner) (string, error) {
	token, err := runner.ReadToken(ctx, d)
	if err != nil {
		return "", fmt.Errorf("read %s broker token from k3s Secret over SSH: %w", poolPrimary, err)
	}
	if token == "" {
		return "", fmt.Errorf("%s broker token is empty", poolPrimary)
	}
	return token, nil
}

func previewBrokerPlan(ctx context.Context, stageDir string, forward brokerForward, token string, stderr io.Writer, runner commandRunner) (migrationPlan, error) {
	var output bytes.Buffer
	if err := runSensitive(ctx, runner, migrateArgv(true, true), migrateEnv(stageDir, token, forward), strings.NewReader(""), &output, stderr, token); err != nil {
		return migrationPlan{}, fmt.Errorf("preview migration to %s broker: %w", poolPrimary, err)
	}
	plan, err := parseMigrationPlan(output.Bytes())
	if err != nil {
		return migrationPlan{}, err
	}
	if !plan.DryRun {
		return migrationPlan{}, fmt.Errorf("%s broker did not return a dry-run migration plan", poolPrimary)
	}
	return plan, nil
}

func uploadCredentials(ctx context.Context, stageDir string, forward brokerForward, token string, d deployment, streams pushStreams, runner commandRunner) error {
	if err := runSensitive(ctx, runner, migrateArgv(false, false), migrateEnv(stageDir, token, forward), streams.stdin, streams.stdout, streams.stderr, token); err != nil {
		return fmt.Errorf("upload credentials to %s broker: %w", poolPrimary, err)
	}
	if err := os.RemoveAll(stageDir); err != nil {
		return fmt.Errorf("remove staged credentials after successful upload: %w", err)
	}
	if _, err := fmt.Fprintf(streams.stdout,
		"Pushed credentials to %s. Add each pushed account to the roster "+
			"with pool = %q, then check the broker on the pool host:\n"+
			"ssh %s kubectl --kubeconfig %s -n %s exec pod/%s -c broker -- env "+
			"OMP_AUTH_BROKER_URL=http://127.0.0.1:%d omp auth-gateway check --json\n",
		poolPrimary, poolPrimary, d.host, d.kubeconfig, d.namespace, d.pod, primaryPort,
	); err != nil {
		return fmt.Errorf("write post-push reminder: %w", err)
	}
	return nil
}

func migrateEnv(stageDir, token string, forward brokerForward) []string {
	set := stagedAgentEnv(stageDir)
	set["OMP_AUTH_BROKER_URL"] = brokerURL(forward.Port())
	set["OMP_AUTH_BROKER_TOKEN"] = token
	return envWith(os.Environ(), set)
}

func printPlan(w io.Writer, plan []credential) error {
	for _, item := range plan {
		if _, err := fmt.Fprintf(w, "%s %s\n", item.Provider, item.Identity); err != nil {
			return fmt.Errorf("write migration plan: %w", err)
		}
	}
	return nil
}

func confirmPush(stdin io.Reader, stdout io.Writer, count int, pool string) (bool, error) {
	if _, err := fmt.Fprintf(stdout, "Push %d credential(s) to %s? [y/N] ", count, pool); err != nil {
		return false, fmt.Errorf("write confirmation prompt: %w", err)
	}
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	answer := strings.TrimSpace(line)
	return strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes"), nil
}

func freeLocalPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("choose local forward port: %w", err)
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		if err := listener.Close(); err != nil {
			return 0, fmt.Errorf("close local port listener: %w", err)
		}
		return 0, errors.New("local port listener returned a non-TCP address")
	}
	port := address.Port
	if err := listener.Close(); err != nil {
		return 0, fmt.Errorf("close local port listener: %w", err)
	}
	return port, nil
}
