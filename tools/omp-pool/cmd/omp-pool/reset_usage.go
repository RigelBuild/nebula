// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
)

type resetUsageOptions struct {
	pool         string
	account      string
	dryRun       bool
	yes          bool
	hostOverride string
	deployment
}

type resetUsageStreams struct {
	stdin  io.Reader
	stdout io.Writer
}

type brokerSnapshot struct {
	Credentials []brokerCredential `json:"credentials"`
}

type brokerCredential struct {
	ID         int    `json:"id"`
	Provider   string `json:"provider"`
	Credential struct {
		Email     string `json:"email"`
		AccountID string `json:"accountId"`
	} `json:"credential"`
}

func parseResetUsageArgs(args []string, getenv func(string) string) (resetUsageOptions, error) {
	if len(args) < 2 {
		return resetUsageOptions{}, usageError{message: "reset-usage requires a pool and account"}
	}
	options := resetUsageOptions{pool: args[0], account: args[1]}
	if !validPool(options.pool) {
		return resetUsageOptions{}, usageError{message: fmt.Sprintf("invalid pool %q", options.pool)}
	}
	if options.account == "" {
		return resetUsageOptions{}, usageError{message: "account must not be empty"}
	}
	args = args[2:]
	for len(args) > 0 {
		consumed, err := parseResetUsageOption(args, &options)
		if err != nil {
			return resetUsageOptions{}, err
		}
		args = args[consumed:]
	}
	d, err := loadDeployment(getenv, options.hostOverride)
	if err != nil {
		return resetUsageOptions{}, err
	}
	options.deployment = d
	return options, nil
}

func parseResetUsageOption(args []string, options *resetUsageOptions) (int, error) {
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
		return 0, usageError{message: fmt.Sprintf("unknown reset-usage option %q", args[0])}
	}
	return 1, nil
}

func runResetUsage(
	parent context.Context,
	args []string,
	getenv func(string) string,
	stdin io.Reader,
	stdout io.Writer,
	runner commandRunner,
) (int, error) {
	options, err := parseResetUsageArgs(args, getenv)
	if err != nil {
		return 2, err
	}
	return executeResetUsage(parent, options, resetUsageStreams{stdin: stdin, stdout: stdout}, runner)
}

func executeResetUsage(parent context.Context, options resetUsageOptions, streams resetUsageStreams, runner commandRunner) (int, error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	forward, err := runner.StartForward(ctx, options.deployment, primaryPort)
	if err != nil {
		return 1, fmt.Errorf("start SSH forward to %s k3s broker: %w", options.pool, err)
	}
	code, runErr := resetUsageWithForward(ctx, options, streams, forward, runner)
	if closeErr := forward.Close(); closeErr != nil {
		return 1, errors.Join(runErr, fmt.Errorf("close SSH forward to %s broker: %w", options.pool, closeErr))
	}
	return code, runErr
}

func resetUsageWithForward(
	ctx context.Context,
	options resetUsageOptions,
	streams resetUsageStreams,
	forward brokerForward,
	runner commandRunner,
) (int, error) {
	baseURL := brokerURL(forward.Port())
	token, credential, err := loadResetTarget(ctx, options, forward, runner)
	if err != nil {
		return 1, err
	}
	if err := writeResetUsagePlan(streams.stdout, options, credential); err != nil {
		return 1, err
	}
	if options.dryRun {
		return 0, nil
	}
	if !options.yes {
		confirmed, err := confirmUsageReset(streams.stdin, streams.stdout, options.pool, options.account)
		if err != nil {
			return 1, err
		}
		if !confirmed {
			return 0, nil
		}
	}
	if err := resetBrokerUsage(ctx, runner, baseURL, token, credential); err != nil {
		return 1, err
	}
	if _, err := fmt.Fprintf(streams.stdout, "Reset cached usage and blocks for %s on %s. Usage refresh will re-poll every %s credential on this broker.\n", options.account, options.pool, credential.Provider); err != nil {
		return 1, fmt.Errorf("write reset usage result: %w", err)
	}
	return 0, nil
}

func loadResetTarget(ctx context.Context, options resetUsageOptions, forward brokerForward, runner commandRunner) (string, brokerCredential, error) {
	if err := forward.WaitReady(ctx); err != nil {
		return "", brokerCredential{}, fmt.Errorf("wait for %s k3s broker forward: %w", options.pool, err)
	}
	token, err := runner.ReadToken(ctx, options.deployment)
	if err != nil {
		return "", brokerCredential{}, fmt.Errorf("read %s broker token from k3s Secret over SSH: %w", options.pool, err)
	}
	if token == "" {
		return "", brokerCredential{}, fmt.Errorf("%s broker token is empty", options.pool)
	}
	baseURL := brokerURL(forward.Port())
	data, err := runner.RequestBroker(ctx, "GET", baseURL, token, "/v1/snapshot")
	if err != nil {
		return "", brokerCredential{}, fmt.Errorf("read %s broker credentials: %w", options.pool, err)
	}
	credential, err := resolveBrokerCredential(data, options.account, options.pool)
	if err != nil {
		return "", brokerCredential{}, err
	}
	return token, credential, nil
}

func resetBrokerUsage(ctx context.Context, runner commandRunner, baseURL, token string, credential brokerCredential) error {
	blocksPath := "/v1/credential/" + strconv.Itoa(credential.ID) + "/blocks"
	if _, err := runner.RequestBroker(ctx, "DELETE", baseURL, token, blocksPath); err != nil {
		return fmt.Errorf("clear blocks for credential %d: %w", credential.ID, err)
	}
	usagePath := "/v1/usage/stale?provider=" + url.QueryEscape(credential.Provider)
	if _, err := runner.RequestBroker(ctx, "POST", baseURL, token, usagePath); err != nil {
		return fmt.Errorf("invalidate %s usage cache after clearing credential blocks: %w", credential.Provider, err)
	}
	return nil
}

func resolveBrokerCredential(data []byte, account, pool string) (brokerCredential, error) {
	var snapshot brokerSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return brokerCredential{}, fmt.Errorf("parse broker credential snapshot: %w", err)
	}
	if snapshot.Credentials == nil {
		return brokerCredential{}, errors.New("broker credential snapshot is missing credentials")
	}
	match, err := findBrokerCredential(snapshot.Credentials, account, pool)
	if err != nil {
		return brokerCredential{}, err
	}
	if match.ID <= 0 {
		return brokerCredential{}, fmt.Errorf("broker credential for account %q has invalid id", account)
	}
	if match.Provider == "" {
		return brokerCredential{}, fmt.Errorf("broker credential for account %q has no provider", account)
	}
	return match, nil
}

func findBrokerCredential(credentials []brokerCredential, account, pool string) (brokerCredential, error) {
	var match brokerCredential
	matched := false
	for _, credential := range credentials {
		if !matchesBrokerAccount(credential, account) {
			continue
		}
		if matched {
			return brokerCredential{}, fmt.Errorf("account %q matches multiple broker credentials on %s broker", account, pool)
		}
		match = credential
		matched = true
	}
	if !matched {
		return brokerCredential{}, fmt.Errorf("account %q not found on %s broker", account, pool)
	}
	return match, nil
}

func matchesBrokerAccount(credential brokerCredential, account string) bool {
	return (credential.Credential.Email != "" && strings.EqualFold(account, credential.Credential.Email)) ||
		(credential.Credential.AccountID != "" && account == credential.Credential.AccountID)
}

func writeResetUsagePlan(stdout io.Writer, options resetUsageOptions, credential brokerCredential) error {
	if options.dryRun {
		_, err := fmt.Fprintf(stdout, "Dry run — would clear credential %d blocks and invalidate %s usage on %s. Usage invalidation re-polls every %s credential on that broker.\n", credential.ID, credential.Provider, options.pool, credential.Provider)
		if err != nil {
			return fmt.Errorf("write reset usage plan: %w", err)
		}
		return nil
	}
	_, err := fmt.Fprintf(stdout, "Reset cached usage and blocks for %s (credential %d, provider %s) on %s. Usage invalidation re-polls every %s credential on that broker.\n", options.account, credential.ID, credential.Provider, options.pool, credential.Provider)
	if err != nil {
		return fmt.Errorf("write reset usage plan: %w", err)
	}
	return nil
}

func confirmUsageReset(stdin io.Reader, stdout io.Writer, pool, account string) (bool, error) {
	if _, err := fmt.Fprintf(stdout, "Reset cached usage for %s on %s? [y/N] ", account, pool); err != nil {
		return false, fmt.Errorf("write reset usage confirmation prompt: %w", err)
	}
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read reset usage confirmation: %w", err)
	}
	answer := strings.TrimSpace(line)
	return strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes"), nil
}
