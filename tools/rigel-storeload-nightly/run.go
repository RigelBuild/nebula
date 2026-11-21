// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec" //nolint:depguard // This tool invokes jj, nix and cotal directly without a shell.
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/credentialfile"
	"github.com/RigelBuild/nebula/tools/go-lib/linearauth"
)

const (
	rigelNightlyTimeout = 50 * time.Minute
	cotalTimeout        = 10 * time.Second
	linearURL           = "https://api.linear.app/graphql"
	nightlyLane         = "storeload-nightly"
	issueLookupAttempts = 3
	issueLookupBase     = 2 * time.Second
	mainOrigin          = "main@origin"
)

var (
	rpcMeasurementPattern  = regexp.MustCompile(`(?:^|\s)store load rpc=(\S+) n=(\S+) p50=(\S+) p99=(\S+)`)
	landMeasurementPattern = regexp.MustCompile(`(?:^|\s)store load land cycles=(\S+) landed=(\S+) p50=(\S+) max=(\S+) interval=(\S+)`)
	rbuildStatusPattern    = regexp.MustCompile(`^rbuild: gat(ing|ed) commit `)
	commitPattern          = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
)

type nightlyConfig struct {
	source       string
	clone        string
	host         string
	task         string
	issue        string
	mention      string
	cotalChannel string
	cotalDir     string
	credentials  linearauth.Credentials
}

type processResult struct {
	exitCode int
	stdout   string
	stderr   string
}

type processOptions struct {
	cwd     string
	env     map[string]string
	timeout time.Duration
}

type processRunner func(context.Context, string, []string, processOptions) (processResult, error)

type rpcMeasurement struct {
	name string
	n    string
	p50  string
	p99  string
}

type landCycleMeasurement struct {
	cycles   string
	landed   string
	p50      string
	max      string
	interval string
}

type measurements struct {
	rpc        []rpcMeasurement
	landCycles *landCycleMeasurement
}

type nightlyResult struct {
	exitCode     int
	commit       string
	measurements measurements
	failureLines []string
	commentBody  string
}
type nightlyDeps struct {
	runner    processRunner
	client    *http.Client
	linearURL string
	now       func() time.Time
	sleep     func(context.Context, time.Duration) error
	log       func(string)
}

func parseMeasurements(output string) measurements {
	parsed := measurements{rpc: []rpcMeasurement{}}
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if matches := rpcMeasurementPattern.FindStringSubmatch(line); len(matches) == 5 {
			parsed.rpc = append(parsed.rpc, rpcMeasurement{name: matches[1], n: matches[2], p50: matches[3], p99: matches[4]})
			continue
		}
		if matches := landMeasurementPattern.FindStringSubmatch(line); len(matches) == 6 {
			parsed.landCycles = &landCycleMeasurement{cycles: matches[1], landed: matches[2], p50: matches[3], max: matches[4], interval: matches[5]}
		}
	}
	return parsed
}

func failureLines(output string) []string {
	failures := []string{}
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.Contains(line, "exceeds") || strings.Contains(line, "--- FAIL") {
			failures = append(failures, line)
		}
	}
	return failures
}

func lastErrorLine(stderr string) string {
	for _, line := range slices.Backward(strings.Split(stderr, "\n")) {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line != "" && !rbuildStatusPattern.MatchString(line) {
			return line
		}
	}
	return ""

}

func objectValue(value any, key string) any {
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	return object[key]
}

func responseError(payload any) string {
	object, ok := payload.(map[string]any)
	if !ok {
		return ""
	}
	errorsValue, ok := object["errors"].([]any)
	if !ok || len(errorsValue) == 0 {
		return ""
	}
	first, ok := errorsValue[0].(map[string]any)
	if !ok {
		return "Linear GraphQL request failed"
	}
	message, ok := first["message"].(string)
	if !ok {
		return "Linear GraphQL request failed"
	}
	return message
}

func linearRequest(ctx context.Context, query, accessToken string, deps nightlyDeps) (any, error) {
	return linearRequestTo(ctx, deps.linearURL, query, accessToken, deps.client)
}

func linearRequestTo(ctx context.Context, endpoint, query, accessToken string, client *http.Client) (any, error) {
	body, err := json.Marshal(struct {
		Query string `json:"query"`
	}{Query: query})
	if err != nil {
		return nil, fmt.Errorf("encoding Linear GraphQL request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating Linear GraphQL request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+accessToken)
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("sending Linear GraphQL request: %w", err)
	}
	responseBody, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, fmt.Errorf("reading Linear GraphQL response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("linear GraphQL returned HTTP %d", response.StatusCode)
	}
	var payload any
	if err := json.Unmarshal(responseBody, &payload); err != nil {
		return nil, fmt.Errorf("decoding Linear GraphQL response: %w", err)
	}
	if message := responseError(payload); message != "" {
		return nil, errors.New(message)
	}
	return payload, nil
}

func findIssueID(ctx context.Context, issue, accessToken string, deps nightlyDeps) (string, error) {
	encodedIssue, err := jsonString(issue)
	if err != nil {
		return "", fmt.Errorf("encoding Linear issue id: %w", err)
	}
	query := "query { issue(id: " + encodedIssue + ") { id } }"
	for attempt := 1; ; attempt++ {
		payload, err := linearRequest(ctx, query, accessToken, deps)
		if err == nil {
			issueID, _ := objectValue(objectValue(objectValue(payload, "data"), "issue"), "id").(string)
			if issueID == "" {
				return "", fmt.Errorf("linear issue %s was not found", issue)
			}
			return issueID, nil
		}
		if err.Error() != "Entity not found: Issue" || attempt >= issueLookupAttempts {
			return "", err
		}
		delay := issueLookupBase * time.Duration(attempt)
		deps.log(fmt.Sprintf("Linear issue lookup attempt %d/%d failed: %s; retrying in %ds", attempt, issueLookupAttempts, err, int(delay.Seconds())))
		if sleepErr := deps.sleep(ctx, delay); sleepErr != nil {
			return "", sleepErr
		}
	}
}

func jsonString(value string) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func commentOnIssue(ctx context.Context, issue, body, accessToken string, deps nightlyDeps) error {
	issueID, err := findIssueID(ctx, issue, accessToken, deps)
	if err != nil {
		return err
	}
	encodedID, err := jsonString(issueID)
	if err != nil {
		return fmt.Errorf("encoding Linear issue id: %w", err)
	}
	encodedBody, err := jsonString(body)
	if err != nil {
		return fmt.Errorf("encoding Linear comment body: %w", err)
	}
	query := "mutation { commentCreate(input: { issueId: " + encodedID + ", body: " + encodedBody + " }) { success } }"
	payload, err := linearRequest(ctx, query, accessToken, deps)
	if err != nil {
		return err
	}
	comment, ok := objectValue(objectValue(payload, "data"), "commentCreate").(map[string]any)
	if !ok || comment["success"] != true {
		return errors.New("linear commentCreate did not succeed")
	}
	return nil
}

func renderComment(commit string, rc int, parsed measurements, failures []string, startedAt time.Time) string {
	rpcRows := "| (none) | — | — | — | — |"
	if len(parsed.rpc) > 0 {
		rows := make([]string, 0, len(parsed.rpc))
		for _, rpc := range parsed.rpc {
			rows = append(rows, fmt.Sprintf("| %s | %s | %s | %s | — |", rpc.name, rpc.n, rpc.p50, rpc.p99))
		}
		rpcRows = strings.Join(rows, "\n")
	}
	landRow := "| Land cycles | (none) | — | — | — |"
	if parsed.landCycles != nil {
		land := parsed.landCycles
		landRow = fmt.Sprintf("| Land cycles | %s/%s | %s | %s | %s |", land.cycles, land.landed, land.p50, land.max, land.interval)
	}
	failureSection := ""
	if len(failures) > 0 {
		rows := make([]string, 0, len(failures))
		for _, line := range failures {
			rows = append(rows, "- "+line)
		}
		failureSection = "\n\nFailure lines:\n" + strings.Join(rows, "\n")
	}
	return fmt.Sprintf("Store-load latency run at %s on commit %s; rc=%d.\n\n| RPC / measurement | n | p50 | p99 / max | interval |\n| --- | ---: | ---: | ---: | ---: |\n%s\n%s%s", startedAt.UTC().Format("2006-01-02T15:04:05.000Z"), shortCommit(commit), rc, rpcRows, landRow, failureSection)
}

func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func runProcess(ctx context.Context, runner processRunner, args []string, options processOptions) (processResult, error) {
	result, err := runner(ctx, "jj", args, options)
	if err != nil {
		return processResult{}, fmt.Errorf("jj %s failed: %w", strings.Join(args, " "), err)
	}
	if result.exitCode != 0 {
		return processResult{}, fmt.Errorf("jj %s exited with rc=%d: %s", strings.Join(args, " "), result.exitCode, strings.TrimSpace(firstNonempty(result.stderr, result.stdout)))
	}
	return result, nil
}

func prepareClone(ctx context.Context, config nightlyConfig, runner processRunner) (string, error) {
	if _, err := os.Stat(config.clone); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(config.clone), 0o750); err != nil {
			return "", fmt.Errorf("creating clone parent: %w", err)
		}
		if _, err := runProcess(ctx, runner, []string{"git", "clone", config.source, config.clone}, processOptions{}); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", fmt.Errorf("checking clone path: %w", err)
	} else if _, err := runProcess(ctx, runner, []string{"git", "fetch"}, processOptions{cwd: config.clone}); err != nil {
		return "", err
	}
	if _, err := runProcess(ctx, runner, []string{"new", mainOrigin}, processOptions{cwd: config.clone}); err != nil {
		return "", err
	}
	resolved, err := runProcess(ctx, runner, []string{"log", "--no-graph", "-r", mainOrigin, "-T", "commit_id"}, processOptions{cwd: config.clone})
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(resolved.stdout)
	if !commitPattern.MatchString(commit) {
		return "", fmt.Errorf("jj did not return a 40-character commit id for %s (got %q)", mainOrigin, commit)
	}
	return commit, nil
}

func processFailureMessage(err error) string {
	if err == nil {
		return "nightly runner failed"
	}
	return err.Error()
}

func runNightly(ctx context.Context, config nightlyConfig, deps nightlyDeps) nightlyResult {
	startedAt := deps.now()
	commit, rc, parsed, failures := runNightlyBenchmark(ctx, config, deps)
	commentBody := renderComment(commit, rc, parsed, failures, startedAt)
	return reportRun(ctx, config, deps, commit, rc, parsed, failures, commentBody)

}

func runNightlyBenchmark(ctx context.Context, config nightlyConfig, deps nightlyDeps) (string, int, measurements, []string) {
	commit := "unknown"
	rc := 1
	parsed := measurements{rpc: []rpcMeasurement{}}
	resolved, err := prepareClone(ctx, config, deps.runner)
	if err != nil {
		return commit, rc, parsed, []string{processFailureMessage(err)}
	}
	commit = resolved
	benchmark, err := deps.runner(ctx, "nix", []string{"run", ".#rbuild", "--", "--hosts", config.host, "--rev", mainOrigin, "--", "moon", "run", config.task}, processOptions{cwd: config.clone, env: map[string]string{"RBUILD_LANE": nightlyLane}, timeout: rigelNightlyTimeout})
	if err != nil {
		return commit, rc, parsed, []string{processFailureMessage(err)}
	}
	rc = benchmark.exitCode
	output := benchmark.stdout + "\n" + benchmark.stderr
	parsed = parseMeasurements(output)
	failures := failureLines(output)
	if rc == 0 && parsed.landCycles == nil {
		failures = append(failures, "no store load land-cycle measurements parsed")
	}
	if rc != 0 && len(failures) == 0 {
		last := lastErrorLine(benchmark.stderr)
		if last == "" {
			last = fmt.Sprintf("benchmark command exited with rc=%d", rc)
		}
		failures = append(failures, last)
	}
	return commit, rc, parsed, failures
}

func reportRun(ctx context.Context, config nightlyConfig, deps nightlyDeps, commit string, rc int, parsed measurements, failures []string, commentBody string) nightlyResult {
	reportFailed := false
	var token linearauth.Token
	tokenMinted := false
	if config.credentials.ClientID == "" || config.credentials.ClientSecret == "" {
		failures = append(failures, "Linear report failed: Linear app credentials are empty")
		reportFailed = true
	} else {
		minted, mintErr := linearauth.MintAppToken(ctx, deps.client, config.credentials, "read,write")
		if mintErr != nil {
			failures = append(failures, "Linear report failed: "+processFailureMessage(mintErr))
			reportFailed = true
		} else {
			token = minted
			tokenMinted = true
			if err := commentOnIssue(ctx, config.issue, commentBody, token.AccessToken, deps); err != nil {
				failures = append(failures, "Linear report failed: "+processFailureMessage(err))
				reportFailed = true
			}
		}
	}
	if tokenMinted {
		if err := linearauth.RevokeAppToken(ctx, deps.client, config.credentials, token); err != nil {
			failures = append(failures, "Linear token revocation failed: "+processFailureMessage(err))
			reportFailed = true
		}
	}

	cotalFailed := false
	failed := rc != 0 || len(failures) > 0 || reportFailed
	if failed {
		first := "nightly run failed"
		if len(failures) > 0 {
			first = failures[0]
		}
		text := fmt.Sprintf("storeload latency nightly FAILED on %s (rc=%d): %s — numbers on %s %s", shortCommit(commit), rc, strings.Join(strings.Fields(first), " "), config.issue, config.mention)
		result, err := deps.runner(ctx, "cotal", []string{"send", "--channel", config.cotalChannel, text, "--space", "main"}, processOptions{cwd: config.cotalDir, timeout: cotalTimeout})
		if err != nil {
			failures = append(failures, "cotal failure report failed: "+processFailureMessage(err))
			cotalFailed = true
		} else if result.exitCode != 0 {
			failures = append(failures, fmt.Sprintf("cotal failure report exited with rc=%d: %s", result.exitCode, strings.TrimSpace(result.stderr)))
			cotalFailed = true
		}
	}
	exitCode := 0
	if rc != 0 || len(failures) > 0 || reportFailed || cotalFailed {
		exitCode = 1
	}
	return nightlyResult{exitCode: exitCode, commit: commit, measurements: parsed, failureLines: failures, commentBody: commentBody}
}

func runSystemProcess(ctx context.Context, command string, args []string, options processOptions) (processResult, error) {
	processCtx := ctx
	cancel := func() {}
	if options.timeout > 0 {
		processCtx, cancel = context.WithTimeout(ctx, options.timeout)
	}
	defer cancel()
	cmd := exec.CommandContext(processCtx, command, args...) //nolint:gosec // G204: commands and arguments are internal tool constants.
	cmd.Dir = options.cwd
	cmd.Env = mergedEnvironment(os.Environ(), options.env)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() >= 0 {
			return processResult{exitCode: exitErr.ExitCode(), stdout: stdout.String(), stderr: stderr.String()}, nil
		}
		return processResult{}, fmt.Errorf("running %s: %w", command, err)
	}
	return processResult{stdout: stdout.String(), stderr: stderr.String()}, nil
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

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func writeResult(stdout, stderr io.Writer, result nightlyResult) error {
	if _, err := fmt.Fprintln(stdout, result.commentBody); err != nil {
		return fmt.Errorf("writing nightly report: %w", err)
	}
	if len(result.failureLines) > 0 {
		if _, err := fmt.Fprintln(stderr, strings.Join(result.failureLines, "\n")); err != nil {
			return fmt.Errorf("writing nightly failures: %w", err)
		}
	}
	return nil
}

func configFromEnvironment(getenv func(string) string) (nightlyConfig, error) {
	config := nightlyConfig{
		source:       getenv("RIGEL_NIGHTLY_SOURCE"),
		clone:        getenv("RIGEL_NIGHTLY_CLONE"),
		host:         getenv("RIGEL_NIGHTLY_HOST"),
		task:         getenv("RIGEL_NIGHTLY_TASK"),
		issue:        getenv("RIGEL_NIGHTLY_ISSUE"),
		mention:      getenv("RIGEL_NIGHTLY_MENTION"),
		cotalChannel: getenv("RIGEL_NIGHTLY_COTAL_CHANNEL"),
		cotalDir:     getenv("RIGEL_NIGHTLY_COTAL_DIR"),
	}
	clientID, hasClientID := credentialfile.Read(getenv("LINEAR_CLIENT_ID_FILE"))
	clientSecret, hasClientSecret := credentialfile.Read(getenv("LINEAR_CLIENT_SECRET_FILE"))
	if config.source == "" || config.clone == "" || config.host == "" || config.task == "" || config.issue == "" || config.mention == "" || config.cotalChannel == "" || config.cotalDir == "" || !hasClientID || !hasClientSecret {
		return nightlyConfig{}, errors.New("RIGEL_NIGHTLY_SOURCE, RIGEL_NIGHTLY_CLONE, RIGEL_NIGHTLY_HOST, RIGEL_NIGHTLY_TASK, RIGEL_NIGHTLY_ISSUE, RIGEL_NIGHTLY_MENTION, RIGEL_NIGHTLY_COTAL_CHANNEL, RIGEL_NIGHTLY_COTAL_DIR, LINEAR_CLIENT_ID_FILE, and LINEAR_CLIENT_SECRET_FILE are required")
	}
	config.credentials = linearauth.Credentials{ClientID: clientID, ClientSecret: clientSecret}
	return config, nil
}
