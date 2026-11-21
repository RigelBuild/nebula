package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type runCall struct {
	name string
	args []string
	env  []string
}
type brokerRequestCall struct {
	method  string
	baseURL string
	path    string
	token   string
}

type fakeRunner struct {
	calls                 []runCall
	tokenSecrets          []string
	tokenHosts            []string
	plans                 map[string]string
	tokens                map[string]string
	forwards              []int
	forwardPods           []string
	forwardHosts          []string
	requests              []brokerRequestCall
	requestErrors         map[int]error
	snapshot              string
	snapshotAfterMutation string
	mutated               bool
	uploaded              bool
	failRun               string
	failUpload            bool
	output                string
	stderr                string
	chunks                []string
}

func (f *fakeRunner) RunOMP(
	_ context.Context,
	args []string,
	env []string,
	_ io.Reader,
	stdout, stderr io.Writer,
) error {
	f.calls = append(f.calls, runCall{name: commandOMP, args: append([]string(nil), args...), env: append([]string(nil), env...)})
	if err := f.commandFailure(args, stderr); err != nil {
		return err
	}
	if len(f.chunks) > 0 {
		return f.writeChunks(stdout, f.chunks)
	}
	if f.output != "" {
		_, err := io.WriteString(stdout, f.output)
		return err
	}
	if !slices.Contains(args, "--dry-run") {
		f.uploaded = true
		_, err := io.WriteString(stdout, "upload complete\n")
		return err
	}
	url := envValue(env, "OMP_AUTH_BROKER_URL")
	_, err := io.WriteString(stdout, f.plans[url])
	return err
}

func (f *fakeRunner) ReadToken(_ context.Context, d deployment) (string, error) {
	f.tokenHosts = append(f.tokenHosts, d.host)
	f.tokenSecrets = append(f.tokenSecrets, d.secret)
	return f.tokens[d.secret], nil
}
func (f *fakeRunner) RequestBroker(_ context.Context, method, baseURL, token, path string) ([]byte, error) {
	f.requests = append(f.requests, brokerRequestCall{method: method, baseURL: baseURL, path: path, token: token})
	if err := f.requestErrors[len(f.requests)-1]; err != nil {
		return nil, err
	}
	if method == "GET" && path == "/v1/snapshot" {
		if f.mutated && f.snapshotAfterMutation != "" {
			return []byte(f.snapshotAfterMutation), nil
		}
		return []byte(f.snapshot), nil
	}
	if method == "POST" && strings.HasSuffix(path, "/disable") {
		f.mutated = true
	}
	return []byte(`{"ok":true}`), nil
}
func (f *fakeRunner) StartForward(_ context.Context, d deployment, remotePort int) (brokerForward, error) {
	localPort := 51000 + len(f.forwards)
	f.forwardHosts = append(f.forwardHosts, d.host)
	f.forwardPods = append(f.forwardPods, d.pod)
	f.forwards = append(f.forwards, remotePort)
	return fakeForward{port: localPort}, nil
}

func (f *fakeRunner) commandFailure(args []string, stderr io.Writer) error {
	if f.failUpload && !slices.Contains(args, "--dry-run") {
		if _, err := io.WriteString(stderr, "upload failed"); err != nil {
			return fmt.Errorf("write simulated upload error: %w", err)
		}
		return os.ErrPermission
	}
	if f.failRun != "" && strings.Contains(strings.Join(args, " "), f.failRun) {
		if _, err := io.WriteString(stderr, f.stderr); err != nil {
			return fmt.Errorf("write simulated command error: %w", err)
		}
		return os.ErrPermission
	}
	return nil
}

func (f *fakeRunner) writeChunks(stdout io.Writer, chunks []string) error {
	for _, chunk := range chunks {
		if _, err := io.WriteString(stdout, chunk); err != nil {
			return err
		}
	}
	return nil
}

type fakeForward struct{ port int }

func (f fakeForward) Port() int                     { return f.port }
func (fakeForward) WaitReady(context.Context) error { return nil }
func (fakeForward) Close() error                    { return nil }

func envValue(env []string, key string) string {
	for _, value := range env {
		name, v, ok := strings.Cut(value, "=")
		if ok && name == key {
			return v
		}
	}
	return ""
}

func stagedPool(t *testing.T, pool string) string {
	t.Helper()
	root := t.TempDir()
	dir := poolStageDir(root, pool)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.db"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func emptyPlan() string {
	return `{"dryRun":true,"plan":[],"skipped":[]}`
}

func plan(provider, identity string) string {
	return `{"dryRun":true,"plan":[{"source":"local","provider":"` + provider + `","identity":"` + identity + `"}],"skipped":[]}`
}

func TestParseLoginArgs(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		wantPool     string
		wantProvider string
		wantErr      bool
	}{
		{name: "primary pool defaults to anthropic", args: []string{poolPrimary}, wantPool: poolPrimary, wantProvider: providerDefault},
		{name: "rejects non-primary pool", args: []string{"pool-x"}, wantErr: true},
		{name: "rejects legacy shared pool name", args: []string{"shared"}, wantErr: true},
		{name: "rejects legacy isolated pool name", args: []string{"isolated"}, wantErr: true},
		{name: "unknown pool", args: []string{"third"}, wantErr: true},
		{name: "too many arguments", args: []string{"primary", "anthropic", "extra"}, wantErr: true},
		{name: "rejects existing pool label", args: []string{"existing"}, wantErr: true},
		{name: "rejects new pool label", args: []string{"new"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool, provider, err := parseLoginArgs(tt.args)
			if (err != nil) != tt.wantErr || pool != tt.wantPool || provider != tt.wantProvider {
				t.Fatalf("parseLoginArgs() = (%q, %q, %v), want (%q, %q, err=%v)", pool, provider, err, tt.wantPool, tt.wantProvider, tt.wantErr)
			}
		})
	}
}
func TestParseResetUsageArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		env     map[string]string
		want    resetUsageOptions
		wantErr bool
	}{
		{name: "defaults from environment", args: []string{poolPrimary, "alice@example.com"}, want: resetUsageOptions{pool: poolPrimary, account: "alice@example.com", deployment: testDeployment()}},
		{name: "flags and host override", args: []string{poolPrimary, "acct-1", "--host", "example-user@other-host", "--yes", "--dry-run"}, want: resetUsageOptions{pool: poolPrimary, account: "acct-1", hostOverride: "example-user@other-host", yes: true, dryRun: true, deployment: deploymentWithHost("example-user@other-host")}},
		{name: "missing account", args: []string{poolPrimary}, wantErr: true},
		{name: "invalid pool", args: []string{"shared", "acct-1"}, wantErr: true},
		{name: "unknown option", args: []string{poolPrimary, "acct-1", "--verbose"}, wantErr: true},
		{name: "missing host", args: []string{poolPrimary, "acct-1", "--host"}, wantErr: true},
		{name: "missing required input", args: []string{poolPrimary, "acct-1"}, env: map[string]string{envNamespace: ""}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseResetUsageArgs(tt.args, testEnv(tt.env))
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("parseResetUsageArgs() = (%+v, %v), want (%+v, err=%v)", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

// deploymentWithHost is testDeployment with its host replaced.
func deploymentWithHost(host string) deployment {
	d := testDeployment()
	d.host = host
	return d
}

func TestRunResetUsage(t *testing.T) {
	for _, test := range resetUsageCases() {
		t.Run(test.name, func(t *testing.T) {
			assertResetUsageCase(t, test)
		})
	}
}

type resetUsageCase struct {
	name            string
	pool            string
	account         string
	args            []string
	snapshot        string
	wantError       string
	wantRequests    []brokerRequestCall
	wantForwardPort int
	wantOutput      string
	stdin           string
}

func resetUsageCases() []resetUsageCase {
	primaryToken := "primary-token-secret"
	return []resetUsageCase{
		{
			name:            "clears credential blocks then provider usage cache",
			pool:            poolPrimary,
			account:         "alice@example.com",
			snapshot:        `{"credentials":[{"id":12,"provider":"anthropic","credential":{"type":"oauth","email":"alice@example.com","accountId":"acct-1"}}]}`,
			wantForwardPort: primaryPort,
			wantRequests: []brokerRequestCall{
				{method: "GET", baseURL: brokerURL(51000), path: "/v1/snapshot", token: primaryToken},
				{method: "DELETE", baseURL: brokerURL(51000), path: "/v1/credential/12/blocks", token: primaryToken},
				{method: "POST", baseURL: brokerURL(51000), path: "/v1/usage/stale?provider=anthropic", token: primaryToken},
			},
			wantOutput: "re-poll every anthropic credential",
		},
		{
			name:            "unknown account does not mutate",
			pool:            poolPrimary,
			account:         "missing@example.com",
			snapshot:        `{"credentials":[{"id":12,"provider":"anthropic","credential":{"type":"oauth","email":"alice@example.com"}}]}`,
			wantError:       "not found on primary broker",
			wantForwardPort: primaryPort,
			wantRequests:    []brokerRequestCall{{method: "GET", baseURL: brokerURL(51000), path: "/v1/snapshot", token: primaryToken}},
		},
		{
			name:            "dry run does not mutate",
			pool:            poolPrimary,
			account:         "acct-1",
			args:            []string{"--dry-run"},
			snapshot:        `{"credentials":[{"id":12,"provider":"anthropic","credential":{"type":"oauth","accountId":"acct-1"}}]}`,
			wantForwardPort: primaryPort,
			wantRequests:    []brokerRequestCall{{method: "GET", baseURL: brokerURL(51000), path: "/v1/snapshot", token: primaryToken}},
			wantOutput:      "Dry run",
		},
		{
			name:            "declined confirmation does not mutate",
			pool:            poolPrimary,
			account:         "acct-1",
			stdin:           "n\n",
			snapshot:        `{"credentials":[{"id":12,"provider":"anthropic","credential":{"type":"oauth","accountId":"acct-1"}}]}`,
			wantForwardPort: primaryPort,
			wantRequests:    []brokerRequestCall{{method: "GET", baseURL: brokerURL(51000), path: "/v1/snapshot", token: primaryToken}},
		},
	}
}

func assertResetUsageCase(t *testing.T, test resetUsageCase) {
	t.Helper()
	fake := resetUsageFake(test.snapshot)
	args := append([]string{test.pool, test.account}, test.args...)
	stdin := "yes\n"
	if test.stdin != "" {
		stdin = test.stdin
	}
	var stdout strings.Builder
	code, err := runResetUsage(context.Background(), args, testEnv(nil), strings.NewReader(stdin), &stdout, fake)
	assertResetUsageResult(t, code, err, test.wantError)
	assertResetUsageRouting(t, fake, test.wantForwardPort)
	if !slices.Equal(fake.requests, test.wantRequests) {
		t.Fatalf("broker requests = %+v, want %+v", fake.requests, test.wantRequests)
	}
	if test.wantOutput != "" && !strings.Contains(stdout.String(), test.wantOutput) {
		t.Fatalf("stdout %q does not contain %q", stdout.String(), test.wantOutput)
	}
}

func assertResetUsageResult(t *testing.T, code int, err error, wantError string) {
	t.Helper()
	if wantError == "" {
		if code != 0 || err != nil {
			t.Fatalf("runResetUsage() = (%d, %v), want success", code, err)
		}
		return
	}
	if code != 1 || err == nil || !strings.Contains(err.Error(), wantError) {
		t.Fatalf("runResetUsage() = (%d, %v), want error containing %q", code, err, wantError)
	}
}

func assertResetUsageRouting(t *testing.T, fake *fakeRunner, port int) {
	t.Helper()
	if !slices.Equal(fake.forwards, []int{port}) {
		t.Fatalf("forward ports = %v, want [%d]", fake.forwards, port)
	}
	if !slices.Equal(fake.forwardPods, []string{testPod}) {
		t.Fatalf("forward pods = %v, want [%s]", fake.forwardPods, testPod)
	}
	if !slices.Equal(fake.forwardHosts, []string{testHost}) || !slices.Equal(fake.tokenHosts, []string{testHost}) {
		t.Fatalf("host routing = forwards %v, token reads %v", fake.forwardHosts, fake.tokenHosts)
	}
	if !slices.Equal(fake.tokenSecrets, []string{testSecret}) {
		t.Fatalf("token Secrets = %v, want [%s]", fake.tokenSecrets, testSecret)
	}
}

func TestRunCommandDispatchesResetUsage(t *testing.T) {
	fake := resetUsageFake(`{"credentials":[{"id":12,"provider":"anthropic","credential":{"type":"oauth","accountId":"acct-1"}}]}`)
	var stdout, stderr strings.Builder
	code := runWithRunner(context.Background(), []string{"reset-usage", poolPrimary, "acct-1", "--dry-run"}, testEnv(nil), strings.NewReader(""), &stdout, &stderr, fake)
	if code != 0 {
		t.Fatalf("runWithRunner() = %d, stderr %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Dry run") || len(fake.requests) != 1 {
		t.Fatalf("stdout %q, requests %+v; want dry-run plan and snapshot only", stdout.String(), fake.requests)
	}
}

func TestResetUsageRejectsAmbiguousAccount(t *testing.T) {
	fake := resetUsageFake(`{"credentials":[{"id":1,"provider":"anthropic","credential":{"type":"oauth","email":"same@example.com"}},{"id":2,"provider":"anthropic","credential":{"type":"oauth","email":"same@example.com"}}]}`)
	_, err := runResetUsage(context.Background(), []string{poolPrimary, "same@example.com", "--yes"}, testEnv(nil), strings.NewReader(""), io.Discard, fake)
	if err == nil || !strings.Contains(err.Error(), "matches multiple") {
		t.Fatalf("runResetUsage() error = %v, want ambiguous account error", err)
	}
	if len(fake.requests) != 1 || fake.requests[0].method != "GET" {
		t.Fatalf("broker requests = %+v, want snapshot only", fake.requests)
	}
}

func TestResolveBrokerCredentialUsesEmailAndAccountID(t *testing.T) {
	data := []byte(`{"credentials":[{"id":1,"provider":"anthropic","credential":{"type":"oauth","email":"Alice@example.com","accountId":"acct-1"}}]}`)
	tests := []struct {
		name    string
		account string
	}{
		{name: "email ignores case", account: "alice@example.com"},
		{name: "account ID exact match", account: "acct-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveBrokerCredential(data, tt.account, poolPrimary)
			if err != nil || got.ID != 1 || got.Provider != "anthropic" {
				t.Fatalf("resolveBrokerCredential() = (%+v, %v), want credential id 1", got, err)
			}
		})
	}
}

func resetUsageFake(snapshot string) *fakeRunner {
	return &fakeRunner{tokens: map[string]string{testSecret: "primary-token-secret"}, snapshot: snapshot}
}

func TestParsePushArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		env     map[string]string
		want    pushOptions
		wantErr bool
	}{
		{name: "defaults from environment", args: []string{poolPrimary}, want: pushOptions{pool: poolPrimary, deployment: testDeployment()}},
		{name: "flag overrides environment host", args: []string{poolPrimary, "--host", "example-user@other-host", "--yes", "--dry-run"}, want: pushOptions{pool: poolPrimary, hostOverride: "example-user@other-host", yes: true, dryRun: true, deployment: deploymentWithHost("example-user@other-host")}},
		{name: "rejects legacy shared pool name", args: []string{"shared"}, wantErr: true},
		{name: "rejects legacy isolated pool name", args: []string{"isolated"}, wantErr: true},
		{name: "unknown option", args: []string{poolPrimary, "--verbose"}, wantErr: true},
		{name: "missing host", args: []string{poolPrimary, "--host"}, wantErr: true},
		{name: "rejects existing pool label", args: []string{"existing"}, wantErr: true},
		{name: "rejects new pool label", args: []string{"new"}, wantErr: true},
		{name: "missing required input", args: []string{poolPrimary}, env: map[string]string{envKubeconfig: ""}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			options, err := parsePushArgs(tt.args, testEnv(tt.env))
			if (err != nil) != tt.wantErr || options != tt.want {
				t.Fatalf("parsePushArgs() = (%+v, %v), want (%+v, err=%v)", options, err, tt.want, tt.wantErr)
			}
		})
	}
}

type pushTestCase struct {
	name          string
	pool          string
	args          []string
	stdin         string
	plans         map[string]string
	wantCode      int
	wantError     string
	wantOut       string
	wantPoolLabel string
	wantUpload    bool
	failUpload    bool
	checkStage    bool
	checkRemoved  bool
}

func checkRunOutcome(code int, err error, tt pushTestCase) error {
	if code != tt.wantCode {
		return fmt.Errorf("runPush() code = %d, want %d", code, tt.wantCode)
	}
	if tt.wantError == "" && err != nil {
		return fmt.Errorf("runPush() error = %v, want nil", err)
	}
	if tt.wantError != "" && (err == nil || !strings.Contains(err.Error(), tt.wantError)) {
		return fmt.Errorf("runPush() error = %v, want substring %q", err, tt.wantError)
	}
	return nil
}

func checkNoTokenLeak(stdout, stderr string, calls []runCall) error {
	if strings.Contains(stdout+stderr, "token-secret") {
		return fmt.Errorf("broker token leaked to output: stdout=%q stderr=%q", stdout, stderr)
	}
	for _, call := range calls {
		for _, arg := range call.args {
			if strings.Contains(arg, "token-secret") {
				return fmt.Errorf("broker token appeared in argv: %q", call.args)
			}
		}
	}
	return nil
}

func TestRunPush(t *testing.T) {
	for _, tt := range pushTestCases(brokerURL(51000)) {
		t.Run(tt.name, func(t *testing.T) {
			runPushTestCase(t, tt)
		})
	}
}

func pushTestCases(primaryURL string) []pushTestCase {
	return []pushTestCase{
		{
			name: "API key identity is refused", pool: poolPrimary,
			plans:    map[string]string{primaryURL: plan("anthropic", "(api key)")},
			wantCode: 1, wantError: "non-OAuth credential",
		},
		{
			name: "empty plan is successful and does not upload", pool: poolPrimary,
			plans:   map[string]string{primaryURL: emptyPlan()},
			wantOut: "nothing to push",
		},
		{
			name: "dry run does not upload", pool: poolPrimary, args: []string{"--dry-run"},
			plans:   map[string]string{primaryURL: plan("anthropic", "alice")},
			wantOut: "alice", checkStage: true,
		},
		{
			name: "confirmation no aborts", pool: poolPrimary, stdin: "n\n",
			plans:      map[string]string{primaryURL: plan("anthropic", "alice")},
			wantOut:    "Push 1 credential(s)",
			checkStage: true,
		},
		{
			name: "primary upload prints roster mapping", pool: poolPrimary, args: []string{"--yes"},
			plans:         map[string]string{primaryURL: plan("anthropic", "alice")},
			wantOut:       "upload complete",
			checkRemoved:  true,
			wantPoolLabel: "primary",
			wantUpload:    true,
		},
		{
			name: "upload failure retains staged database", pool: poolPrimary, args: []string{"--yes"},
			plans:    map[string]string{primaryURL: plan("anthropic", "alice")},
			wantCode: 1, wantError: "upload credentials", wantOut: "alice", failUpload: true, checkStage: true,
		},
	}
}

func runPushTestCase(t *testing.T, tt pushTestCase) {
	t.Helper()
	root := stagedPool(t, tt.pool)
	fake := &fakeRunner{
		plans:      tt.plans,
		tokens:     map[string]string{testSecret: "primary-token-secret"},
		failUpload: tt.failUpload,
	}
	args := append([]string{tt.pool}, tt.args...)
	getenv := testEnv(map[string]string{"OMP_POOL_STAGE_DIR": root})
	var stdout, stderr strings.Builder
	code, err := runPush(context.Background(), args, getenv, strings.NewReader(tt.stdin), &stdout, &stderr, fake)
	if err := checkRunOutcome(code, err, tt); err != nil {
		t.Fatal(err)
	}
	if fake.uploaded != tt.wantUpload {
		t.Fatalf("uploaded = %v, want %v", fake.uploaded, tt.wantUpload)
	}
	if !strings.Contains(stdout.String(), tt.wantOut) {
		t.Fatalf("stdout %q does not contain %q", stdout.String(), tt.wantOut)
	}
	assertPushBrokerAuth(t, fake.calls)
	assertPoolMappingOutput(t, stdout.String(), tt.wantPoolLabel)
	if err := checkNoTokenLeak(stdout.String(), stderr.String(), fake.calls); err != nil {
		t.Fatal(err)
	}
	if tt.checkRemoved {
		assertStagedDatabaseRemoved(t, root, tt.pool)
	}
	if tt.checkStage {
		assertStagedDirectoryRemains(t, root, tt.pool)
	}
}

func assertPushBrokerAuth(t *testing.T, calls []runCall) {
	t.Helper()
	if len(calls) == 0 {
		t.Fatal("push made no broker calls")
	}
	for _, call := range calls {
		if envValue(call.env, "OMP_AUTH_BROKER_URL") == "" {
			t.Fatalf("broker URL missing from environment for %v", call.args)
		}
		if envValue(call.env, "OMP_AUTH_BROKER_TOKEN") != "primary-token-secret" {
			t.Fatalf("primary broker token missing from environment for %v", call.args)
		}
	}
}

func assertPoolMappingOutput(t *testing.T, output, wantPoolLabel string) {
	t.Helper()
	if wantPoolLabel == "" {
		return
	}
	wantRoster := fmt.Sprintf("with pool = %q", wantPoolLabel)
	if !strings.Contains(output, wantRoster) {
		t.Fatalf("stdout %q does not contain roster instruction %q", output, wantRoster)
	}
	wantCheck := fmt.Sprintf(
		"ssh %s kubectl --kubeconfig %s -n %s exec pod/%s -c broker -- env OMP_AUTH_BROKER_URL=http://127.0.0.1:%d omp auth-gateway check --json",
		testHost, testKubeconfig, testNamespace, testPod, primaryPort,
	)
	if !strings.Contains(output, wantCheck) {
		t.Fatalf("stdout %q does not contain broker check command %q", output, wantCheck)
	}
}

func assertStagedDirectoryRemains(t *testing.T, root, pool string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(poolStageDir(root, pool), "agent.db")); err != nil {
		t.Fatalf("staged database was removed: %v", err)
	}
}

func assertStagedDatabaseRemoved(t *testing.T, root, pool string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(poolStageDir(root, pool), "agent.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged database stat error = %v, want not-exist", err)
	}
}

func TestRunPushRejectsMissingStagedDatabaseBeforeNetwork(t *testing.T) {
	root := t.TempDir()
	fake := &fakeRunner{}
	var stdout, stderr strings.Builder
	code, err := runPush(context.Background(), []string{poolPrimary, "--dry-run"}, testEnv(map[string]string{"OMP_POOL_STAGE_DIR": root}), strings.NewReader(""), &stdout, &stderr, fake)
	if err == nil || code != 1 || len(fake.forwards) != 0 {
		t.Fatalf("runPush() = (%d, %v), forwards %v; want missing-stage failure before network", code, err, fake.forwards)
	}
}

func TestRunLoginRemovesBrokerCredentials(t *testing.T) {
	root := t.TempDir()
	fake := &fakeRunner{}
	var stdout, stderr strings.Builder
	getenv := func(key string) string {
		if key == "OMP_POOL_STAGE_DIR" {
			return root
		}
		return ""
	}
	code, err := runLogin(context.Background(), []string{poolPrimary}, getenv, strings.NewReader(""), &stdout, &stderr, fake)
	if err := assertLoginSafe(code, err, fake.calls, root); err != nil {
		t.Fatal(err)
	}
}

func assertLoginSafe(code int, err error, calls []runCall, root string) error {
	if err != nil || code != 0 || len(calls) != 1 {
		return fmt.Errorf("runLogin() = (%d, %v), calls %d", code, err, len(calls))
	}
	call := calls[0]
	if call.name != commandOMP || strings.Join(call.args, " ") != "auth-broker login anthropic" {
		return fmt.Errorf("login command = %s %v", call.name, call.args)
	}
	if envValue(call.env, "OMP_AUTH_BROKER_URL") != "" || envValue(call.env, "OMP_AUTH_BROKER_TOKEN") != "" {
		return fmt.Errorf("broker auth leaked into login environment: %v", call.env)
	}
	info, err := os.Stat(filepath.Join(root, poolPrimary))
	if err != nil {
		return err
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("staging directory mode = %o, want 700", info.Mode().Perm())
	}
	return nil
}

func TestRunSensitiveRedactsSplitSecret(t *testing.T) {
	var stdout, stderr strings.Builder
	runner := &fakeRunner{chunks: []string{"token-", "secret"}}
	err := runSensitive(context.Background(), runner, nil, nil, strings.NewReader(""), &stdout, &stderr, "token-secret")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String()+stderr.String(), "token-secret") || !strings.Contains(stdout.String(), "[redacted]") {
		t.Fatalf("sensitive output not redacted: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestRunSensitiveRedactsFailureOutput(t *testing.T) {
	var stdout, stderr strings.Builder
	runner := &fakeRunner{failRun: "migrate", stderr: "token-secret"}
	err := runSensitive(context.Background(), runner, []string{"migrate"}, nil, strings.NewReader(""), &stdout, &stderr, "token-secret")
	if err == nil {
		t.Fatal("runSensitive() error = nil, want command failure")
	}
	if strings.Contains(stdout.String()+stderr.String()+err.Error(), "token-secret") || stderr.String() != "[redacted]" {
		t.Fatalf("sensitive output not redacted: stdout=%q stderr=%q error=%q", stdout.String(), stderr.String(), err)
	}
}

func TestEnvBuildersSetAndRemoveBrokerSettings(t *testing.T) {
	for _, profileKey := range []string{"OMP_PROFILE", "PI_PROFILE"} {
		t.Run(profileKey, func(t *testing.T) {
			t.Setenv("OMP_PROFILE", "")
			t.Setenv("PI_PROFILE", "")
			t.Setenv(profileKey, "inherited-profile")
			t.Setenv("OMP_AUTH_BROKER_URL", "https://old")
			t.Setenv("OMP_AUTH_BROKER_TOKEN", "old-token")

			base := os.Environ()
			login := loginEnv(base, "/stage/primary")
			if envValue(login, "PI_CODING_AGENT_DIR") != "/stage/primary" || envValue(login, "OMP_AUTH_BROKER_URL") != "" || envValue(login, "OMP_AUTH_BROKER_TOKEN") != "" {
				t.Fatalf("login env = %v", login)
			}
			assertDefaultProfile(t, login)
			migration := migrateEnv("/stage/primary", "primary-token", fakeForward{port: primaryPort})
			if envValue(migration, "PI_CODING_AGENT_DIR") != "/stage/primary" || envValue(migration, "OMP_AUTH_BROKER_URL") != brokerURL(primaryPort) || envValue(migration, "OMP_AUTH_BROKER_TOKEN") != "primary-token" {
				t.Fatalf("migration env = %v", migration)
			}
			assertDefaultProfile(t, migration)
		})
	}
}

func assertDefaultProfile(t *testing.T, env []string) {
	t.Helper()
	for _, key := range []string{"OMP_PROFILE", "PI_PROFILE"} {
		if got := envValue(env, key); got != "default" {
			t.Errorf("%s = %q, want \"default\" so no dotenv file can select a profile", key, got)
		}
	}
}

func TestRunStatusReportsPrimaryPoolWithoutNetwork(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(poolStageDir(root, poolPrimary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(poolStageDir(root, poolPrimary), "agent.db"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout strings.Builder
	code, err := runStatus(nil, func(key string) string {
		if key == "OMP_POOL_STAGE_DIR" {
			return root
		}
		return ""
	}, &stdout)
	if err != nil || code != 0 {
		t.Fatalf("runStatus() = (%d, %v)", code, err)
	}
	if got, want := stdout.String(), "primary: staged login exists\n"; got != want {
		t.Fatalf("status output = %q, want %q", got, want)
	}
}

func TestRunStatusRejectsArguments(t *testing.T) {
	code, err := runStatus([]string{"removed-pool"}, func(string) string { return t.TempDir() }, io.Discard)
	if code != 2 || err == nil {
		t.Fatalf("runStatus() = (%d, %v), want usage error", code, err)
	}
}
