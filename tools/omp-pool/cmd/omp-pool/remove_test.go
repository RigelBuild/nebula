package main

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

type failOnReadStdin struct {
	t *testing.T
}

func (r failOnReadStdin) Read([]byte) (int, error) {
	r.t.Helper()
	r.t.Errorf("dry-run read stdin")
	return 0, io.ErrUnexpectedEOF
}

func TestParseRemoveArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		env     map[string]string
		want    removeOptions
		wantErr bool
	}{
		{
			name: "defaults from environment",
			args: []string{poolPrimary, "alice@example.com"},
			want: removeOptions{pool: poolPrimary, account: "alice@example.com", deployment: testDeployment()},
		},
		{
			name: "flags and host override",
			args: []string{poolPrimary, "acct-1", "--host", "example-user@other-host", "--yes", "--dry-run"},
			want: removeOptions{pool: poolPrimary, account: "acct-1", hostOverride: "example-user@other-host", yes: true, dryRun: true, deployment: deploymentWithHost("example-user@other-host")},
		},
		{name: "missing account", args: []string{poolPrimary}, wantErr: true},
		{name: "invalid pool", args: []string{"shared", "acct-1"}, wantErr: true},
		{name: "unknown option", args: []string{poolPrimary, "acct-1", "--verbose"}, wantErr: true},
		{name: "missing host", args: []string{poolPrimary, "acct-1", "--host"}, wantErr: true},
		{name: "missing required input", args: []string{poolPrimary, "acct-1"}, env: map[string]string{envBrokerSecret: ""}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseRemoveArgs(tt.args, testEnv(tt.env))
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("parseRemoveArgs() = (%+v, %v), want (%+v, err=%v)", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

type removeTestCase struct {
	name                  string
	account               string
	args                  []string
	stdin                 string
	noReadStdin           bool
	snapshot              string
	snapshotAfterMutation string
	requestErrors         map[int]error
	wantErr               string
	wantOutput            string
	wantNoOutput          string
	wantMethods           []string
	wantPaths             []string
}

func TestRunRemove(t *testing.T) {
	primaryToken := "primary-token-secret"
	tests := append(removeSuccessfulCases(), removeFailClosedCases()...)
	tests = append(tests, removeVerificationFailureCases()...)
	tests = append(tests, removeRequestFailureCases()...)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertRunRemoveCase(t, tt, primaryToken)
		})
	}
}

func removeSuccessfulCases() []removeTestCase {
	return []removeTestCase{
		{
			name:                  "disables and verifies with a fresh snapshot",
			account:               "alice@example.com",
			snapshot:              `{"credentials":[{"id":12,"provider":"anthropic","credential":{"email":"alice@example.com","accountId":"acct-1"}}]}`,
			snapshotAfterMutation: `{"credentials":[]}`,
			wantOutput:            "Soft-disabled credential 12",
			wantMethods:           []string{"GET", "POST", "GET"},
			wantPaths:             []string{"/v1/snapshot", "/v1/credential/12/disable", "/v1/snapshot"},
		},
		{
			name:         "dry run does not mutate or confirm",
			account:      "acct-1",
			args:         []string{flagDryRun},
			noReadStdin:  true,
			snapshot:     `{"credentials":[{"id":12,"provider":"anthropic","credential":{"accountId":"acct-1"}}]}`,
			wantOutput:   "Dry run",
			wantNoOutput: "Remove account",
			wantMethods:  []string{"GET"},
			wantPaths:    []string{"/v1/snapshot"},
		},
		{
			name:        "declined confirmation does not mutate",
			account:     "acct-1",
			stdin:       "n\n",
			snapshot:    `{"credentials":[{"id":12,"provider":"anthropic","credential":{"accountId":"acct-1"}}]}`,
			wantOutput:  "Remove account",
			wantMethods: []string{"GET"},
			wantPaths:   []string{"/v1/snapshot"},
		},
	}
}

func removeFailClosedCases() []removeTestCase {
	return []removeTestCase{
		{
			name:        "unknown account does not mutate",
			account:     "missing@example.com",
			snapshot:    `{"credentials":[{"id":12,"provider":"anthropic","credential":{"email":"alice@example.com"}}]}`,
			wantErr:     "not found on primary broker",
			wantMethods: []string{"GET"},
			wantPaths:   []string{"/v1/snapshot"},
		},
		{
			name:        "ambiguous account does not mutate",
			account:     "same@example.com",
			snapshot:    `{"credentials":[{"id":1,"provider":"anthropic","credential":{"email":"same@example.com"}},{"id":2,"provider":"anthropic","credential":{"email":"same@example.com"}}]}`,
			args:        []string{flagYes},
			wantErr:     "matches multiple",
			wantMethods: []string{"GET"},
			wantPaths:   []string{"/v1/snapshot"},
		},
	}
}

func removeVerificationFailureCases() []removeTestCase {
	return []removeTestCase{
		{
			name:                  "still present in fresh snapshot is a failure",
			account:               "acct-1",
			snapshot:              `{"credentials":[{"id":12,"provider":"anthropic","credential":{"accountId":"acct-1"}}]}`,
			snapshotAfterMutation: `{"credentials":[{"id":12,"provider":"anthropic","credential":{"accountId":"acct-1"}}]}`,
			args:                  []string{flagYes},
			wantErr:               "still present in the broker snapshot",
			wantNoOutput:          "verified absent from a fresh snapshot",
			wantMethods:           []string{"GET", "POST", "GET"},
			wantPaths:             []string{"/v1/snapshot", "/v1/credential/12/disable", "/v1/snapshot"},
		},
		{
			name:                  "malformed fresh snapshot is a failure",
			account:               "acct-1",
			snapshot:              `{"credentials":[{"id":12,"provider":"anthropic","credential":{"accountId":"acct-1"}}]}`,
			snapshotAfterMutation: `{`,
			args:                  []string{flagYes},
			wantErr:               "parse broker credential snapshot after removal",
			wantNoOutput:          "verified absent from a fresh snapshot",
			wantMethods:           []string{"GET", "POST", "GET"},
			wantPaths:             []string{"/v1/snapshot", "/v1/credential/12/disable", "/v1/snapshot"},
		},
	}
}

func removeRequestFailureCases() []removeTestCase {
	return []removeTestCase{
		{
			name:          "failed disable request stops before verification",
			account:       "acct-1",
			args:          []string{flagYes},
			snapshot:      `{"credentials":[{"id":12,"provider":"anthropic","credential":{"accountId":"acct-1"}}]}`,
			requestErrors: map[int]error{1: errors.New("disable request failed")},
			wantErr:       "disable credential 12",
			wantNoOutput:  "verified absent from a fresh snapshot",
			wantMethods:   []string{"GET", "POST"},
			wantPaths:     []string{"/v1/snapshot", "/v1/credential/12/disable"},
		},
		{
			name:          "failed verification snapshot is a failure",
			account:       "acct-1",
			args:          []string{flagYes},
			snapshot:      `{"credentials":[{"id":12,"provider":"anthropic","credential":{"accountId":"acct-1"}}]}`,
			requestErrors: map[int]error{2: errors.New("verification request failed")},
			wantErr:       "verify removed credential",
			wantNoOutput:  "verified absent from a fresh snapshot",
			wantMethods:   []string{"GET", "POST", "GET"},
			wantPaths:     []string{"/v1/snapshot", "/v1/credential/12/disable", "/v1/snapshot"},
		},
	}
}

func assertRunRemoveCase(t *testing.T, tt removeTestCase, primaryToken string) {
	t.Helper()
	fake := &fakeRunner{
		tokens:                map[string]string{testSecret: primaryToken},
		snapshot:              tt.snapshot,
		snapshotAfterMutation: tt.snapshotAfterMutation,
		requestErrors:         tt.requestErrors,
	}
	args := append([]string{poolPrimary, tt.account}, tt.args...)
	stdin := tt.stdin
	if stdin == "" {
		stdin = "yes\n"
	}
	var input io.Reader = strings.NewReader(stdin)
	if tt.noReadStdin {
		input = failOnReadStdin{t: t}
	}
	var stdout strings.Builder
	code, err := runRemove(
		context.Background(),
		args,
		testEnv(nil),
		input,
		&stdout,
		fake,
	)
	assertRemoveRunResult(t, tt, code, err, stdout.String(), primaryToken)
	assertRemoveRunRequests(t, tt, fake, primaryToken)
}

func assertRemoveRunResult(t *testing.T, tt removeTestCase, code int, err error, output, token string) {
	t.Helper()
	assertRemoveRunStatus(t, tt, code, err)
	assertRemoveRunOutput(t, tt.wantOutput, tt.wantNoOutput, output)
	assertRemoveRunTokenIsHidden(t, output, err, token)
}

func assertRemoveRunStatus(t *testing.T, tt removeTestCase, code int, err error) {
	t.Helper()
	if tt.wantErr == "" {
		if code != 0 || err != nil {
			t.Fatalf("runRemove() = (%d, %v), want success", code, err)
		}
		return
	}
	if code != 1 || err == nil || !strings.Contains(err.Error(), tt.wantErr) {
		t.Fatalf("runRemove() = (%d, %v), want error containing %q", code, err, tt.wantErr)
	}
}

func assertRemoveRunOutput(t *testing.T, wantOutput, wantNoOutput, output string) {
	t.Helper()
	if wantOutput != "" && !strings.Contains(output, wantOutput) {
		t.Fatalf("stdout %q does not contain %q", output, wantOutput)
	}
	if wantNoOutput != "" && strings.Contains(output, wantNoOutput) {
		t.Fatalf("stdout %q contains unexpected text %q", output, wantNoOutput)
	}
}

func assertRemoveRunTokenIsHidden(t *testing.T, output string, err error, token string) {
	t.Helper()
	if strings.Contains(output, token) || (err != nil && strings.Contains(err.Error(), token)) {
		t.Fatal("broker token leaked into command output")
	}
}

func assertRemoveRunRequests(t *testing.T, tt removeTestCase, fake *fakeRunner, token string) {
	t.Helper()
	methods := make([]string, 0, len(fake.requests))
	paths := make([]string, 0, len(fake.requests))
	for _, request := range fake.requests {
		methods = append(methods, request.method)
		paths = append(paths, request.path)
		if request.token != token {
			t.Fatal("broker request used an unexpected token")
		}
	}
	if !slices.Equal(methods, tt.wantMethods) || !slices.Equal(paths, tt.wantPaths) {
		t.Fatalf("broker requests = methods %v paths %v, want methods %v paths %v", methods, paths, tt.wantMethods, tt.wantPaths)
	}
	if !slices.Equal(fake.forwards, []int{primaryPort}) ||
		!slices.Equal(fake.forwardPods, []string{testPod}) ||
		!slices.Equal(fake.forwardHosts, []string{testHost}) {
		t.Fatalf("forward routing = ports %v pods %v hosts %v", fake.forwards, fake.forwardPods, fake.forwardHosts)
	}
	if !slices.Equal(fake.tokenSecrets, []string{testSecret}) {
		t.Fatalf("token Secrets = %v, want [%s]", fake.tokenSecrets, testSecret)
	}
}

func TestRunCommandDispatchesRemove(t *testing.T) {
	fake := &fakeRunner{
		tokens:                map[string]string{testSecret: "primary-token-secret"},
		snapshot:              `{"credentials":[{"id":12,"provider":"anthropic","credential":{"accountId":"acct-1"}}]}`,
		snapshotAfterMutation: `{"credentials":[]}`,
	}
	var stdout, stderr strings.Builder
	code := runWithRunner(
		context.Background(),
		[]string{"remove", poolPrimary, "acct-1", "--yes"},
		testEnv(nil),
		strings.NewReader(""),
		&stdout,
		&stderr,
		fake,
	)
	if code != 0 {
		t.Fatalf("runWithRunner() code = %d, stderr %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Soft-disabled credential 12") {
		t.Fatalf("stdout %q does not report verified soft-disable", stdout.String())
	}
}

func TestRunRemoveRoutesSelectedPoolAndHost(t *testing.T) {
	const token = "primary-token-secret"
	fake := &fakeRunner{
		tokens:                map[string]string{testSecret: token},
		snapshot:              `{"credentials":[{"id":8,"provider":"anthropic","credential":{"accountId":"acct-2"}}]}`,
		snapshotAfterMutation: `{"credentials":[]}`,
	}
	var stdout strings.Builder
	code, err := runRemove(
		context.Background(),
		[]string{poolPrimary, "acct-2", "--host", "example-user@other-host", "--yes"},
		testEnv(nil),
		strings.NewReader(""),
		&stdout,
		fake,
	)
	if err != nil || code != 0 {
		t.Fatalf("runRemove() = (%d, %v), want success", code, err)
	}
	if !slices.Equal(fake.forwards, []int{primaryPort}) ||
		!slices.Equal(fake.forwardPods, []string{testPod}) ||
		!slices.Equal(fake.forwardHosts, []string{"example-user@other-host"}) ||
		!slices.Equal(fake.tokenHosts, []string{"example-user@other-host"}) {
		t.Fatalf("routing = forward ports %v, pods %v, forward hosts %v, token hosts %v", fake.forwards, fake.forwardPods, fake.forwardHosts, fake.tokenHosts)
	}
	if !slices.Equal(fake.tokenSecrets, []string{testSecret}) {
		t.Fatalf("token Secrets = %v, want [%s]", fake.tokenSecrets, testSecret)
	}
	if strings.Contains(stdout.String(), token) {
		t.Fatal("selected broker token leaked into output")
	}
}

func TestVerifyBrokerCredentialRemoved(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		wantErr bool
	}{
		{name: "credential absent", data: `{"credentials":[]}`},
		{name: "credential remains", data: `{"credentials":[{"id":7}]}`, wantErr: true},
		{name: "missing credentials field", data: `{}`, wantErr: true},
		{name: "malformed snapshot", data: `{`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifyBrokerCredentialRemoved([]byte(tt.data), 7)
			if (err != nil) != tt.wantErr {
				t.Fatalf("verifyBrokerCredentialRemoved() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
