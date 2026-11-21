package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestRunPushUsesOnlyPrimaryBroker(t *testing.T) {
	const host = "example-user@broker-test"
	root := stagedPool(t, poolPrimary)
	fake := &fakeRunner{
		plans:  map[string]string{brokerURL(51000): plan("anthropic", "alice")},
		tokens: map[string]string{testSecret: "primary-token-secret"},
	}
	getenv := testEnv(map[string]string{"OMP_POOL_STAGE_DIR": root})
	var stdout, stderr strings.Builder
	code, err := runPush(context.Background(), []string{poolPrimary, "--host", host, "--yes"}, getenv, strings.NewReader(""), &stdout, &stderr, fake)
	if err != nil || code != 0 {
		t.Fatalf("runPush() = (%d, %v), stderr %q", code, err, stderr.String())
	}
	assertPrimaryPushRouting(t, fake, host)
	assertPushCalls(t, fake.calls)
}

func assertPushCalls(t *testing.T, calls []runCall) {
	t.Helper()
	if len(calls) != 2 {
		t.Fatalf("RunOMP calls = %d, want preview and upload", len(calls))
	}
	if !slices.Contains(calls[0].args, "--dry-run") || slices.Contains(calls[1].args, "--dry-run") {
		t.Fatalf("RunOMP args = %v then %v, want preview then upload", calls[0].args, calls[1].args)
	}
	assertPushRunAuth(t, calls)
}

func assertPrimaryPushRouting(t *testing.T, fake *fakeRunner, host string) {
	t.Helper()
	if !slices.Equal(fake.forwardPods, []string{testPod}) {
		t.Fatalf("forward pods = %v, want only %s", fake.forwardPods, testPod)
	}
	if !slices.Equal(fake.forwardHosts, []string{host}) {
		t.Fatalf("forward hosts = %v, want only %s", fake.forwardHosts, host)
	}
	if !slices.Equal(fake.forwards, []int{primaryPort}) {
		t.Fatalf("forward ports = %v, want only %d", fake.forwards, primaryPort)
	}
	if !slices.Equal(fake.tokenSecrets, []string{testSecret}) {
		t.Fatalf("token Secrets = %v, want only %s", fake.tokenSecrets, testSecret)
	}
	if !slices.Equal(fake.tokenHosts, []string{host}) {
		t.Fatalf("token hosts = %v, want only %s", fake.tokenHosts, host)
	}
}

func assertPushRunAuth(t *testing.T, calls []runCall) {
	t.Helper()
	for _, call := range calls {
		if got := envValue(call.env, "OMP_AUTH_BROKER_URL"); got != brokerURL(51000) {
			t.Fatalf("broker URL = %q, want %q", got, brokerURL(51000))
		}
		if got := envValue(call.env, "OMP_AUTH_BROKER_TOKEN"); got != "primary-token-secret" {
			t.Fatalf("broker token = %q, want primary token", got)
		}
	}
}

func TestParseMigrationPlan(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "empty plan", input: `{"dryRun":true,"plan":[],"skipped":[]}`},
		{name: "plan with credential", input: `{"dryRun":true,"plan":[{"source":"local","provider":"anthropic","identity":"alice"}],"skipped":[]}`},
		{name: "malformed JSON", input: `{`, wantErr: true},
		{name: "missing plan", input: `{"skipped":[]}`, wantErr: true},
		{name: "missing skipped", input: `{"plan":[]}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseMigrationPlan([]byte(tt.input))
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseMigrationPlan() error = %v, want error %v", err, tt.wantErr)
			}
			if !tt.wantErr && !got.DryRun {
				t.Fatalf("dryRun = false, want true")
			}
		})
	}
}

func TestValidateTargetPlan(t *testing.T) {
	for _, test := range []struct {
		name    string
		plan    migrationPlan
		wantErr string
	}{
		{
			name: "OAuth accepted",
			plan: migrationPlan{Plan: []credential{{Provider: "openai", Identity: "alice"}}},
		},
		{
			name:    "API key refused",
			plan:    migrationPlan{Plan: []credential{{Provider: "anthropic", Identity: "(api key)"}}},
			wantErr: "non-OAuth credential",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateTargetPlan(test.plan)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("validateTargetPlan() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("validateTargetPlan() error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestStartBrokerForwardClosesWhenReadinessFails(t *testing.T) {
	readinessErr := errors.New("readiness failed")
	closeErr := errors.New("close failed")
	forward := &readinessTestForward{readyErr: readinessErr, closeErr: closeErr}
	runner := &readinessTestRunner{forward: forward}

	got, err := startBrokerForward(context.Background(), testDeployment(), runner)
	if got != nil || !errors.Is(err, readinessErr) || !errors.Is(err, closeErr) {
		t.Fatalf("startBrokerForward() = (%v, %v), want joined readiness and close errors", got, err)
	}
	if !forward.closed {
		t.Fatal("forward was not closed after readiness failure")
	}
}

type readinessTestRunner struct {
	fakeRunner
	forward brokerForward
}

func (r *readinessTestRunner) StartForward(context.Context, deployment, int) (brokerForward, error) {
	return r.forward, nil
}

type readinessTestForward struct {
	readyErr error
	closeErr error
	closed   bool
}

func (*readinessTestForward) Port() int { return 51000 }

func (f *readinessTestForward) WaitReady(context.Context) error { return f.readyErr }

func (f *readinessTestForward) Close() error {
	f.closed = true
	return f.closeErr
}

func TestMigrateArgv(t *testing.T) {
	tests := []struct {
		name string
		dry  bool
		json bool
		want string
	}{
		{name: "preview JSON", dry: true, json: true, want: "auth-broker migrate --from-local --include-oauth --dry-run --json"},
		{name: "upload", want: "auth-broker migrate --from-local --include-oauth"},
		{name: "preview without JSON", dry: true, want: "auth-broker migrate --from-local --include-oauth --dry-run"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := strings.Join(migrateArgv(tt.dry, tt.json), " ")
			if got != tt.want {
				t.Fatalf("migrateArgv() = %q, want %q", got, tt.want)
			}
		})
	}
}
