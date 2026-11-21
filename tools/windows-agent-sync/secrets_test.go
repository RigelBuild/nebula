package main

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

type fakeEnvStore struct {
	values        map[string]string
	deleted       []string
	sets          []string
	failSet       string
	failDelete    string
	failBroadcast bool
	broadcasts    int
}

func newFakeEnvStore() *fakeEnvStore {
	return &fakeEnvStore{values: make(map[string]string)}
}

func (s *fakeEnvStore) Get(name string) (string, error) {
	return s.values[name], nil
}

func (s *fakeEnvStore) Set(name, value string) error {
	if name == s.failSet {
		return errors.New("set failed")
	}
	s.values[name] = value
	s.sets = append(s.sets, name)
	return nil
}

func (s *fakeEnvStore) Delete(name string) error {
	if name == s.failDelete {
		return errors.New("delete failed")
	}
	delete(s.values, name)
	s.deleted = append(s.deleted, name)
	return nil
}

func (s *fakeEnvStore) Broadcast() error {
	s.broadcasts++
	if s.failBroadcast {
		return errors.New("broadcast failed")
	}
	return nil
}

type closeableFakeEnvStore struct {
	*fakeEnvStore
	closeErr error
	closed   bool
}

func (s *closeableFakeEnvStore) Close() error {
	s.closed = true
	return s.closeErr
}

type fakeRunner struct {
	outputs map[string][]byte
	calls   [][]string
	failAt  int
}

func (r *fakeRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	call := append([]string{name}, args...)
	r.calls = append(r.calls, call)
	if r.failAt > 0 && len(r.calls) == r.failAt {
		return nil, errors.New("runner failed")
	}
	if len(args) < 5 {
		return nil, errors.New("unexpected runner args")
	}
	return r.outputs[args[len(args)-1]], nil
}

func (r *fakeRunner) Runner() Runner {
	return r.Run
}

func fixtureSecretMap() SecretMap {
	return SecretMap{
		Team: []secretRef{
			{Var: "WOODPECKER_TOKEN", Key: "example-team-token"},
			{Var: "GITHUB_PERSONAL_ACCESS_TOKEN", Key: "example-scoped-token", PersonalOnly: true},
		},
		LiteLLM: liteLLMMap{
			BaseURL: secretRef{Var: "LITELLM_BASE_URL", Key: "example-base-url"},
			APIKey:  secretRef{Var: "LITELLM_API_KEY", Key: "example-api-key"},
		},
		Linear: secretRef{Var: "LINEAR_API_KEY", Key: "example-linear-key"},
		Search: []secretRef{
			{Var: "EXA_API_KEY", Key: "example-search-one"},
			{Var: "PARALLEL_API_KEY", Key: "example-search-two"},
			{Var: "BRAVE_API_KEY", Key: "example-search-three"},
		},
		FileExports: []secretRef{{Var: "DL_CLAIM_TOKEN_FILE", Key: "example-file-token"}},
		Literals: map[string]string{
			"WOODPECKER_SERVER": "https://ci.example.test",
			"LINEAR_TEAM_ID":    "EX",
		},
		ExportedVarNames: []string{
			"WOODPECKER_TOKEN", "GITHUB_PERSONAL_ACCESS_TOKEN", "LITELLM_BASE_URL", "LITELLM_API_KEY", "LITELLM_MCP_URL",
			"LINEAR_API_KEY", "EXA_API_KEY", "PARALLEL_API_KEY", "BRAVE_API_KEY", "DL_CLAIM_TOKEN_FILE",
			"WOODPECKER_SERVER", "LINEAR_TEAM_ID",
		},
	}
}

func fixtureRunner() *fakeRunner {
	return &fakeRunner{outputs: map[string][]byte{
		"/run/secrets/example-team-token":   []byte("woodpecker-secret\r\n"),
		"/run/secrets/example-scoped-token": []byte("github-secret\r\n"),
		"/run/secrets/example-base-url":     []byte("https://llm.example/v1\n"),
		"/run/secrets/example-api-key":      []byte("litellm-secret\r\n"),
		"/run/secrets/example-linear-key":   []byte("linear-secret\n"),
		"/run/secrets/example-search-one":   []byte("exa-secret\r\n"),
		"/run/secrets/example-search-two":   []byte("parallel-secret\n"),
		"/run/secrets/example-search-three": []byte("brave-secret\r\n"),
		"/run/secrets/example-file-token":   []byte("claim-secret\n"),
	}}
}

func TestToWSLPath(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
		wantErr  bool
	}{
		{name: "drive path", input: `C:\Work\example\agents\repo`, expected: "/mnt/c/Work/example/agents/repo"},
		{name: "lowercase drive", input: `d:/src/repo`, expected: "/mnt/d/src/repo"},
		{name: "root path", input: `C:\`, expected: "/mnt/c"},
		{name: "relative path", input: `agents\repo`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actual, err := toWSLPath(test.input)
			if (err != nil) != test.wantErr {
				t.Fatalf("toWSLPath(%q) error = %v, wantErr %v", test.input, err, test.wantErr)
			}
			if actual != test.expected {
				t.Fatalf("toWSLPath(%q) = %q, want %q", test.input, actual, test.expected)
			}
		})
	}
}

func TestLoadMap(t *testing.T) {
	output := []byte(`{"team":[],"exportedVarNames":[]}`)
	runner := &fakeRunner{outputs: map[string][]byte{"/mnt/c/map.nix": output}}
	loaded, err := loadMap(context.Background(), runner.Runner(), "NixOS", "/mnt/c/map.nix")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Team) != 0 || len(loaded.ExportedVarNames) != 0 {
		t.Fatalf("loadMap() = %#v", loaded)
	}
}

func TestLoadMapWSLArgv(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{"/mnt/c/map.nix": []byte(`{"team":[],"exportedVarNames":[]}`)}}
	if _, err := loadMap(context.Background(), runner.Runner(), "NixOS", "/mnt/c/map.nix"); err != nil {
		t.Fatal(err)
	}
	want := []string{"wsl.exe", "-d", "NixOS", "--exec", "/run/current-system/sw/bin/nix", "eval", "--json", "--file", "/mnt/c/map.nix"}
	if len(runner.calls) != 1 || !reflect.DeepEqual(runner.calls[0], want) {
		t.Fatalf("loadMap() calls = %q, want one call %q", runner.calls, want)
	}
}

func TestLoadMapFailure(t *testing.T) {
	runner := fixtureRunner()
	runner.failAt = 1
	_, err := loadMap(context.Background(), runner.Runner(), "NixOS", "/mnt/c/map.nix")
	if err == nil || !strings.Contains(err.Error(), "evaluating secret map") {
		t.Fatalf("loadMap() error = %v, want evaluation failure", err)
	}
	for _, value := range runner.outputs {
		secret := strings.TrimSpace(string(value))
		if secret != "" && strings.Contains(err.Error(), secret) {
			t.Fatal("error contains secret value")
		}
	}
}

func TestResolveFailureCases(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*SecretMap, *fakeRunner)
		wantCalls int
		wantError string
	}{
		{
			name: "missing key",
			mutate: func(_ *SecretMap, runner *fakeRunner) {
				runner.failAt = 1
			},
			wantCalls: 1,
			wantError: "example-team-token",
		},
		{
			name: "blank key",
			mutate: func(_ *SecretMap, runner *fakeRunner) {
				runner.outputs["/run/secrets/example-team-token"] = []byte(" \r\n")
			},
			wantCalls: 1,
			wantError: "example-team-token",
		},
		{
			name: "scoped Team key missing",
			mutate: func(_ *SecretMap, runner *fakeRunner) {
				delete(runner.outputs, "/run/secrets/example-scoped-token")
			},
			wantCalls: 2,
			wantError: "example-scoped-token",
		},
		{
			name: "half LiteLLM pair",
			mutate: func(_ *SecretMap, runner *fakeRunner) {
				delete(runner.outputs, "/run/secrets/example-api-key")
			},
			wantCalls: 9,
			wantError: "example-api-key",
		},
		{
			name: "export list mismatch",
			mutate: func(m *SecretMap, _ *fakeRunner) {
				m.ExportedVarNames = m.ExportedVarNames[:len(m.ExportedVarNames)-1]
			},
			wantCalls: 9,
			wantError: "exportedVarNames",
		},
		{
			name: "WSL failure",
			mutate: func(_ *SecretMap, runner *fakeRunner) {
				runner.failAt = 1
			},
			wantCalls: 1,
			wantError: "example-team-token",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			secretMap := fixtureSecretMap()
			runner := fixtureRunner()
			test.mutate(&secretMap, runner)
			vars, files, err := resolve(context.Background(), runner.Runner(), "NixOS", secretMap)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("resolve() error = %v, want substring %q", err, test.wantError)
			}
			if len(runner.calls) != test.wantCalls {
				t.Fatalf("runner calls = %d, want %d", len(runner.calls), test.wantCalls)
			}
			if len(vars) != 0 || len(files) != 0 {
				t.Fatalf("resolve() returned partial values: vars=%v files=%v", keys(vars), keys(files))
			}
			for _, value := range runner.outputs {
				secret := strings.TrimSpace(string(value))
				if secret != "" && strings.Contains(err.Error(), secret) {
					t.Fatal("error contains secret value")
				}
			}
		})
	}
}

func TestResolve(t *testing.T) {
	vars, files, err := resolve(context.Background(), fixtureRunner().Runner(), "NixOS", fixtureSecretMap())
	if err != nil {
		t.Fatal(err)
	}
	if vars["LITELLM_MCP_URL"] != "https://llm.example/mcp/" {
		t.Fatalf("MCP URL = %q", vars["LITELLM_MCP_URL"])
	}
	if vars["WOODPECKER_TOKEN"] != "woodpecker-secret" || files["DL_CLAIM_TOKEN_FILE"] != "claim-secret" {
		t.Fatal("resolved secret destinations are incorrect")
	}
}

func TestResolveWSLArgv(t *testing.T) {
	runner := fixtureRunner()
	if _, _, err := resolve(context.Background(), runner.Runner(), "NixOS", fixtureSecretMap()); err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{
		"example-team-token",
		"example-scoped-token",
		"example-linear-key",
		"example-search-one",
		"example-search-two",
		"example-search-three",
		"example-file-token",
		"example-base-url",
		"example-api-key",
	}
	if len(runner.calls) != len(wantKeys) {
		t.Fatalf("runner calls = %d, want %d", len(runner.calls), len(wantKeys))
	}
	for index, call := range runner.calls {
		want := []string{"wsl.exe", "-d", "NixOS", "--exec", "/run/current-system/sw/bin/cat", path.Join("/run/secrets", wantKeys[index])}
		if !reflect.DeepEqual(call, want) {
			t.Fatalf("resolve() call %d = %q, want %q", index, call, want)
		}
	}
}

func TestMCPURL(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{name: "versioned base", input: "https://litellm.example/v1", expected: "https://litellm.example/mcp/"},
		{name: "versioned base with slash", input: "https://litellm.example/v1/", expected: "https://litellm.example/mcp/"},
		{name: "base with slash", input: "https://litellm.example/", expected: "https://litellm.example/mcp/"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if actual := mcpURL(test.input); actual != test.expected {
				t.Fatalf("mcpURL(%q) = %q, want %q", test.input, actual, test.expected)
			}
		})
	}
}

func TestApplySecretsStagingFailure(t *testing.T) {
	store := newFakeEnvStore()
	store.values["KEEP"] = "original"
	parent := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(parent, []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	fileDir := filepath.Join(parent, "exports")
	_, err := applySecrets(store, map[string]string{"NEW": "value"}, map[string]string{"DL_CLAIM_TOKEN_FILE": "token"}, fileDir, []string{"KEEP"})
	if err == nil {
		t.Fatal("applySecrets() succeeded despite staging error")
	}
	if store.values["KEEP"] != "original" || len(store.sets) != 0 || len(store.deleted) != 0 || store.broadcasts != 0 {
		t.Fatalf("registry changed after staging failure: %#v", store)
	}
}

func TestCommitStagedExportsReplacementFailurePreservesPriorExport(t *testing.T) {
	fileDir := t.TempDir()
	target := filepath.Join(fileDir, exportFileName("DL_CLAIM_TOKEN_FILE"))
	const priorContent = "previous-token"
	if err := os.WriteFile(target, []byte(priorContent), 0o600); err != nil {
		t.Fatal(err)
	}
	priorInfo, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	tempPath := filepath.Join(fileDir, ".windows-agent-sync-failed.tmp")
	const secret = "staged-secret-value"
	if err := os.WriteFile(tempPath, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	staged := map[string]string{"DL_CLAIM_TOKEN_FILE": tempPath}
	replaceErr := errors.Join(errors.New(secret), os.ErrPermission)
	err = commitStagedExportsWithReplace(staged, fileDir, func(string, string) error {
		return replaceErr
	})
	if err == nil || !strings.Contains(err.Error(), "DL_CLAIM_TOKEN_FILE") {
		t.Fatalf("commitStagedExportsWithReplace() error = %v, want replacement failure", err)
	}
	if !errors.Is(err, errExportReplacementFailed) || !errors.Is(err, os.ErrPermission) || errors.Is(err, replaceErr) {
		t.Fatalf("replacement cause = %v; want safe failure and permission classification without arbitrary cause", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("replacement diagnostic exposed staged secret")
	}
	if len(staged) != 0 {
		t.Fatalf("staged exports after failure = %v, want empty", staged)
	}
	if _, err := os.Stat(tempPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged file after failure: %v", err)
	}
	currentInfo, err := os.Stat(target)
	if err != nil || !os.SameFile(priorInfo, currentInfo) {
		t.Fatalf("prior export identity changed after replacement failure: info=%v err=%v", currentInfo, err)
	}
	current, err := os.ReadFile(target)
	if err != nil || string(current) != priorContent {
		t.Fatalf("prior export after replacement failure = %q, %v; want prior contents", current, err)
	}

	if err := os.WriteFile(tempPath, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	staged["DL_CLAIM_TOKEN_FILE"] = tempPath
	if err := commitStagedExportsWithReplace(staged, fileDir, os.Rename); err != nil {
		t.Fatalf("commitStagedExportsWithReplace() retry error = %v", err)
	}
	if len(staged) != 0 {
		t.Fatalf("staged exports after retry = %v, want empty", staged)
	}
	current, err = os.ReadFile(target)
	if err != nil || string(current) != secret {
		t.Fatalf("export after retry = %q, %v; want staged contents", current, err)
	}
	if _, err := os.Stat(tempPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged file after retry: %v", err)
	}
}

func TestApplySecretsFileExportSetFailureConverges(t *testing.T) {
	store := newFakeEnvStore()
	fileDir := t.TempDir()
	store.failSet = "DL_CLAIM_TOKEN_FILE"
	files := map[string]string{"DL_CLAIM_TOKEN_FILE": "staged-secret-token"}

	_, err := applySecrets(store, nil, files, fileDir, []string{"DL_CLAIM_TOKEN_FILE"})
	if err == nil || !strings.Contains(err.Error(), "DL_CLAIM_TOKEN_FILE") {
		t.Fatalf("applySecrets() error = %v, want file variable Set failure", err)
	}
	if got, err := os.ReadFile(filepath.Join(fileDir, "dl-claim-token")); err != nil || string(got) != "staged-secret-token" {
		t.Fatalf("committed export = %q, %v; want new file contents", got, err)
	}
	if temps, err := filepath.Glob(filepath.Join(fileDir, ".windows-agent-sync-*.tmp")); err != nil || len(temps) != 0 {
		t.Fatalf("staged exports after Set failure: %v, %v", temps, err)
	}
	if store.values["DL_CLAIM_TOKEN_FILE"] != "" {
		t.Fatalf("environment path after failed Set = %q, want unset", store.values["DL_CLAIM_TOKEN_FILE"])
	}

	store.failSet = ""
	managed, err := applySecrets(store, nil, files, fileDir, []string{"DL_CLAIM_TOKEN_FILE"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(managed, []string{"DL_CLAIM_TOKEN_FILE"}) || store.values["DL_CLAIM_TOKEN_FILE"] != filepath.Join(fileDir, "dl-claim-token") {
		t.Fatalf("retry did not converge: managed=%v values=%v", managed, store.values)
	}
	if !reflect.DeepEqual(store.sets, []string{"DL_CLAIM_TOKEN_FILE"}) {
		t.Fatalf("file export registry writes = %v, want retry only", store.sets)
	}
}

func TestApplySecretsCommitSetFailureConverges(t *testing.T) {
	store := newFakeEnvStore()
	store.failSet = "B"
	fileDir := t.TempDir()
	vars := map[string]string{"A": "one", "B": "two"}
	_, err := applySecrets(store, vars, map[string]string{}, fileDir, nil)
	if err == nil || !strings.Contains(err.Error(), "B") {
		t.Fatalf("applySecrets() error = %v, want failed variable B", err)
	}
	store.failSet = ""
	managed, err := applySecrets(store, vars, map[string]string{}, fileDir, []string{"A"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(managed, []string{"A", "B"}) || store.values["A"] != "one" || store.values["B"] != "two" {
		t.Fatalf("rerun did not converge: managed=%v values=%v", managed, store.values)
	}
}

func TestApplySecretsBroadcastFailure(t *testing.T) {
	store := newFakeEnvStore()
	store.failBroadcast = true
	_, err := applySecrets(store, map[string]string{"A": "value"}, map[string]string{}, t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "broadcast") {
		t.Fatalf("applySecrets() error = %v, want broadcast failure", err)
	}
}

func TestApplySecretsRetiredDeleteFailure(t *testing.T) {
	store := newFakeEnvStore()
	fileDir := t.TempDir()
	tokenPath := filepath.Join(fileDir, "dl-claim-token")
	if err := os.WriteFile(tokenPath, []byte("previous token"), 0o600); err != nil {
		t.Fatal(err)
	}
	store.values["DL_CLAIM_TOKEN_FILE"] = tokenPath
	store.failDelete = "DL_CLAIM_TOKEN_FILE"
	_, err := applySecrets(store, map[string]string{}, map[string]string{}, fileDir, []string{"DL_CLAIM_TOKEN_FILE"})
	if err == nil || !strings.Contains(err.Error(), "DL_CLAIM_TOKEN_FILE") {
		t.Fatalf("applySecrets() error = %v, want delete failure", err)
	}
	if _, err := os.Stat(tokenPath); err != nil {
		t.Fatalf("old export removed after failed registry delete: %v", err)
	}
	store.failDelete = ""
	managed, err := applySecrets(store, map[string]string{}, map[string]string{}, fileDir, []string{"DL_CLAIM_TOKEN_FILE"})
	if err != nil {
		t.Fatal(err)
	}
	if len(managed) != 0 {
		t.Fatalf("managed = %v, want empty", managed)
	}
	if _, err := os.Stat(tokenPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired export remains: %v", err)
	}
}

func TestApplySecretsManagedSet(t *testing.T) {
	store := newFakeEnvStore()
	store.values["OLD"] = "retired"
	managed, err := applySecrets(store, map[string]string{"NEW": "value"}, map[string]string{}, t.TempDir(), []string{"OLD"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(managed, []string{"NEW"}) {
		t.Fatalf("managed = %v, want [NEW]", managed)
	}
	if _, found := store.values["OLD"]; found {
		t.Fatal("retired variable was not deleted")
	}
}

func TestApplySecretsExportsAndNoop(t *testing.T) {
	store := newFakeEnvStore()
	fileDir := t.TempDir()
	filePath := filepath.Join(fileDir, "dl-claim-token")
	if err := os.WriteFile(filePath, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	store.values["DL_CLAIM_TOKEN_FILE"] = filePath
	managed, err := applySecrets(store, map[string]string{"WOODPECKER_TOKEN": "new"}, map[string]string{"DL_CLAIM_TOKEN_FILE": "token"}, fileDir, []string{"DL_CLAIM_TOKEN_FILE", "OLD"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(managed, []string{"DL_CLAIM_TOKEN_FILE", "WOODPECKER_TOKEN"}) {
		t.Fatalf("managed = %v", managed)
	}
	contents, err := os.ReadFile(filePath)
	if err != nil || string(contents) != "token" {
		t.Fatalf("export = %q, err=%v", contents, err)
	}
	if store.values["DL_CLAIM_TOKEN_FILE"] != filePath || store.values["WOODPECKER_TOKEN"] != "new" {
		t.Fatalf("environment values = %v", store.values)
	}
	setsBefore, broadcastsBefore := len(store.sets), store.broadcasts
	managed, err = applySecrets(store, map[string]string{"WOODPECKER_TOKEN": "new"}, map[string]string{"DL_CLAIM_TOKEN_FILE": "token"}, fileDir, managed)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(managed, []string{"DL_CLAIM_TOKEN_FILE", "WOODPECKER_TOKEN"}) || len(store.sets) != setsBefore || store.broadcasts != broadcastsBefore+1 {
		t.Fatalf("no-op sync changed registry state or did not broadcast: managed=%v sets=%v broadcasts=%d", managed, store.sets, store.broadcasts)
	}
}

func TestApplySecretsExportMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix mode bits differ on Windows")
	}
	fileDir := t.TempDir()
	store := newFakeEnvStore()
	if _, err := applySecrets(store, map[string]string{}, map[string]string{"DL_CLAIM_TOKEN_FILE": "token"}, fileDir, nil); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(fileDir, "dl-claim-token"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("export mode = %o, want 600", mode)
	}
}

func TestApplySecretsRetriesFailedBroadcast(t *testing.T) {
	store := newFakeEnvStore()
	store.failBroadcast = true
	vars := map[string]string{"A": "value"}
	fileDir := t.TempDir()

	if _, err := applySecrets(store, vars, map[string]string{}, fileDir, nil); err == nil || !strings.Contains(err.Error(), "broadcast") {
		t.Fatalf("first applySecrets() error = %v, want broadcast failure", err)
	}

	store.failBroadcast = false
	managed, err := applySecrets(store, vars, map[string]string{}, fileDir, nil)
	if err != nil {
		t.Fatalf("retry applySecrets() error = %v", err)
	}
	if !reflect.DeepEqual(managed, []string{"A"}) || store.broadcasts != 2 || !reflect.DeepEqual(store.sets, []string{"A"}) {
		t.Fatalf("retry did not broadcast without recommitting: managed=%v broadcasts=%d sets=%v", managed, store.broadcasts, store.sets)
	}
}

func keys(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for name := range values {
		result = append(result, name)
	}
	return result
}

func TestFileExportNaming(t *testing.T) {
	for _, tc := range []struct {
		name     string
		wantFile string
		isFile   bool
	}{
		{"DL_CLAIM_TOKEN_FILE", "dl-claim-token", true},
		{"SOPS_AGE_KEY_FILE", "sops-age-key", true},
		{"WOODPECKER_TOKEN", "woodpecker-token", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isFileExportName(tc.name); got != tc.isFile {
				t.Fatalf("isFileExportName(%q) = %v, want %v", tc.name, got, tc.isFile)
			}
			if tc.isFile {
				if got := exportFileName(tc.name); got != tc.wantFile {
					t.Fatalf("exportFileName(%q) = %q, want %q", tc.name, got, tc.wantFile)
				}
			}
		})
	}
}
