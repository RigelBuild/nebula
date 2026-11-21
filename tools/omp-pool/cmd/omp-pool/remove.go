// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

type removeOptions struct {
	pool         string
	account      string
	dryRun       bool
	yes          bool
	hostOverride string
	deployment
}

type removeStreams struct {
	stdin  io.Reader
	stdout io.Writer
}

func parseRemoveArgs(args []string, getenv func(string) string) (removeOptions, error) {
	if len(args) < 2 {
		return removeOptions{}, usageError{message: "remove requires a pool and account"}
	}
	options := removeOptions{pool: args[0], account: args[1]}
	if !validPool(options.pool) {
		return removeOptions{}, usageError{message: fmt.Sprintf("invalid pool %q", options.pool)}
	}
	if options.account == "" {
		return removeOptions{}, usageError{message: "account must not be empty"}
	}
	args = args[2:]
	for len(args) > 0 {
		consumed, err := parseRemoveOption(args, &options)
		if err != nil {
			return removeOptions{}, err
		}
		args = args[consumed:]
	}
	d, err := loadDeployment(getenv, options.hostOverride)
	if err != nil {
		return removeOptions{}, err
	}
	options.deployment = d
	return options, nil
}

func parseRemoveOption(args []string, options *removeOptions) (int, error) {
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
		return 0, usageError{message: fmt.Sprintf("unknown remove option %q", args[0])}
	}
	return 1, nil
}

func runRemove(
	parent context.Context,
	args []string,
	getenv func(string) string,
	stdin io.Reader,
	stdout io.Writer,
	runner commandRunner,
) (int, error) {
	options, err := parseRemoveArgs(args, getenv)
	if err != nil {
		return 2, err
	}
	return executeRemove(parent, options, removeStreams{stdin: stdin, stdout: stdout}, runner)
}

func executeRemove(parent context.Context, options removeOptions, streams removeStreams, runner commandRunner) (int, error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	forward, err := runner.StartForward(ctx, options.deployment, primaryPort)
	if err != nil {
		return 1, fmt.Errorf("start SSH forward to %s k3s broker: %w", options.pool, err)
	}
	code, runErr := removeWithForward(ctx, options, streams, forward, runner)
	if closeErr := forward.Close(); closeErr != nil {
		return 1, errors.Join(runErr, fmt.Errorf("close SSH forward to %s broker: %w", options.pool, closeErr))
	}
	return code, runErr
}

func removeWithForward(
	ctx context.Context,
	options removeOptions,
	streams removeStreams,
	forward brokerForward,
	runner commandRunner,
) (int, error) {
	baseURL := brokerURL(forward.Port())
	token, credential, err := loadRemoveTarget(ctx, options, forward, runner)
	if err != nil {
		return 1, err
	}
	if err := writeRemovePlan(streams.stdout, options, credential); err != nil {
		return 1, err
	}
	if options.dryRun {
		return 0, nil
	}
	if !options.yes {
		confirmed, err := confirmCredentialRemoval(streams.stdin, streams.stdout, options.pool, options.account)
		if err != nil {
			return 1, err
		}
		if !confirmed {
			return 0, nil
		}
	}
	if err := disableBrokerCredential(ctx, runner, baseURL, token, credential); err != nil {
		return 1, err
	}
	if err := verifyRemovedCredential(ctx, runner, baseURL, token, options.pool, credential.ID); err != nil {
		return 1, err
	}
	if _, err := fmt.Fprintf(streams.stdout, "Soft-disabled credential %d on %s broker; verified absent from a fresh snapshot.\n", credential.ID, options.pool); err != nil {
		return 1, fmt.Errorf("write credential removal result: %w", err)
	}
	return 0, nil
}

func verifyRemovedCredential(ctx context.Context, runner commandRunner, baseURL, token, pool string, credentialID int) error {
	snapshot, err := runner.RequestBroker(ctx, "GET", baseURL, token, "/v1/snapshot")
	if err != nil {
		return fmt.Errorf("verify removed credential on %s broker: %w", pool, err)
	}
	return verifyBrokerCredentialRemoved(snapshot, credentialID)
}

func loadRemoveTarget(
	ctx context.Context,
	options removeOptions,
	forward brokerForward,
	runner commandRunner,
) (string, brokerCredential, error) {
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
	data, err := runner.RequestBroker(ctx, "GET", brokerURL(forward.Port()), token, "/v1/snapshot")
	if err != nil {
		return "", brokerCredential{}, fmt.Errorf("read %s broker credentials: %w", options.pool, err)
	}
	credential, err := resolveBrokerCredential(data, options.account, options.pool)
	if err != nil {
		return "", brokerCredential{}, err
	}
	return token, credential, nil
}

func disableBrokerCredential(ctx context.Context, runner commandRunner, baseURL, token string, credential brokerCredential) error {
	path := "/v1/credential/" + strconv.Itoa(credential.ID) + "/disable"
	if _, err := runner.RequestBroker(ctx, "POST", baseURL, token, path); err != nil {
		return fmt.Errorf("disable credential %d on broker: %w", credential.ID, err)
	}
	return nil
}

func verifyBrokerCredentialRemoved(data []byte, id int) error {
	var snapshot brokerSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return fmt.Errorf("parse broker credential snapshot after removal: %w", err)
	}
	if snapshot.Credentials == nil {
		return errors.New("broker credential snapshot after removal is missing credentials")
	}
	for _, credential := range snapshot.Credentials {
		if credential.ID == id {
			return fmt.Errorf("credential %d is still present in the broker snapshot after removal", id)
		}
	}
	return nil
}

func writeRemovePlan(stdout io.Writer, options removeOptions, credential brokerCredential) error {
	if options.dryRun {
		if _, err := fmt.Fprintf(stdout, "Dry run — would disable credential %d on %s broker.\n", credential.ID, options.pool); err != nil {
			return fmt.Errorf("write credential removal plan: %w", err)
		}
		return nil
	}
	if _, err := fmt.Fprintf(stdout, "Will disable credential %d on %s broker.\n", credential.ID, options.pool); err != nil {
		return fmt.Errorf("write credential removal plan: %w", err)
	}
	return nil
}

func confirmCredentialRemoval(stdin io.Reader, stdout io.Writer, pool, account string) (bool, error) {
	if _, err := fmt.Fprintf(stdout, "Remove account %q from %s broker? [y/N] ", account, pool); err != nil {
		return false, fmt.Errorf("write credential removal confirmation prompt: %w", err)
	}
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read credential removal confirmation: %w", err)
	}
	answer := strings.TrimSpace(line)
	return strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes"), nil
}
