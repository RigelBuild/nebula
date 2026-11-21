package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// testLayoutFile is the layout fixture. Its paths are relative to testRepoRoot.
const testLayoutFile = "testdata/layout.cue"

// testRepoRoot is the example Windows checkout that the fixture layout names.
const testRepoRoot = `C:\agents\example`

// testMapPath is the WSL path of the secret map fixture under testRepoRoot.
const testMapPath = "/mnt/c/agents/example/secrets/env-map.nix"

func testArgs() []string {
	return []string{"--repo-root", testRepoRoot, "--layout", testLayoutFile}
}

func testCheckout(t *testing.T) checkout {
	t.Helper()
	paths, err := loadLayout(testLayoutFile)
	if err != nil {
		t.Fatal(err)
	}
	return checkout{root: testRepoRoot, layout: paths}
}

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		localData  string
		wantCode   int
		wantOutput string
	}{
		{name: "requires repo root", wantCode: 1, wantOutput: "--repo-root is required"},
		{name: "requires layout", args: []string{"--repo-root", testRepoRoot}, wantCode: 1, wantOutput: "--layout is required"},
		{name: "requires local app data", args: testArgs(), wantCode: 1, wantOutput: "LOCALAPPDATA is required"},
		{name: "help", args: []string{"--help"}, wantCode: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			getenv := func(name string) string {
				switch name {
				case "USERPROFILE":
					return `C:\Users\me`
				case "APPDATA":
					return `C:\Users\me\AppData\Roaming`
				case "LOCALAPPDATA":
					return test.localData
				default:
					return ""
				}
			}
			code := runWithArgs(context.Background(), getenv, &stdout, &stderr, test.args, nil, nil)
			if code != test.wantCode {
				t.Fatalf("exit code = %d, want %d", code, test.wantCode)
			}
			if test.wantOutput != "" && !strings.Contains(stdout.String()+stderr.String(), test.wantOutput) {
				t.Fatalf("output = %q, want diagnostic containing %q", stdout.String()+stderr.String(), test.wantOutput)
			}
		})
	}
}

func TestRunRecordsCreatedSecretsBeforeApplyingAndConvergesRemovedNames(t *testing.T) {
	localData := t.TempDir()
	statePath := filepath.Join(localData, "rigel", "windows-agent-sync", "state.json")
	old := []string{"OLD_SECRET"}
	if err := writeState(statePath, old); err != nil {
		t.Fatal(err)
	}
	store := newFakeEnvStore()
	store.failSet = "WOODPECKER_SERVER"
	store.values["OLD_SECRET"] = "old value"
	runner := fixtureRunner()
	secretMap := fixtureSecretMap()
	secretMap.Literals["REMOVED_SECRET"] = "source value"
	secretMap.ExportedVarNames = append(secretMap.ExportedVarNames, "REMOVED_SECRET")
	mapOutput, err := json.Marshal(secretMap)
	if err != nil {
		t.Fatal(err)
	}
	runner.outputs[testMapPath] = mapOutput
	var stdout, stderr bytes.Buffer
	getenv := func(name string) string {
		switch name {
		case "USERPROFILE":
			return `C:\Users\me`
		case "APPDATA":
			return `C:\Users\me\AppData\Roaming`
		case "LOCALAPPDATA":
			return localData
		default:
			return ""
		}
	}
	links := func(checkout, string, string) error { return nil }
	binaries := func(context.Context, Runner, EnvStore, checkout, string) error { return nil }
	run := func() int {
		stdout.Reset()
		stderr.Reset()
		return runWithArgsAndPhases(context.Background(), getenv, &stdout, &stderr, testArgs(), runner.Runner(), func() (EnvStore, error) {
			return store, nil
		}, links, binaries)
	}

	if code := run(); code == 0 {
		t.Fatal("run succeeded despite a secret synchronization failure")
	}
	wantManaged := unionNames(old, secretMap.ExportedVarNames)
	if got, err := readState(statePath); err != nil || !reflect.DeepEqual(got, wantManaged) {
		t.Fatalf("state after failed commit = %v, %v; want old and all new names %v", got, err, wantManaged)
	}
	if len(store.sets) == 0 {
		t.Fatal("failure did not occur after creating any secrets")
	}

	store.failSet = ""
	store.values["OLD_SECRET"] = "old value"
	store.values["REMOVED_SECRET"] = "source value"
	secretMap = SecretMap{LiteLLM: liteLLMMap{BaseURL: secretRef{Var: "LITELLM_BASE_URL", Key: "example-base-url"}, APIKey: secretRef{Var: "LITELLM_API_KEY", Key: "example-api-key"}}, Linear: secretRef{Var: "LINEAR_API_KEY", Key: "example-linear-key"}, Literals: map[string]string{"RETAINED_SECRET": "retained value"}, ExportedVarNames: []string{"LITELLM_BASE_URL", "LITELLM_API_KEY", "LITELLM_MCP_URL", "LINEAR_API_KEY", "RETAINED_SECRET"}}
	mapOutput, err = json.Marshal(secretMap)
	if err != nil {
		t.Fatal(err)
	}
	runner.outputs[testMapPath] = mapOutput
	if code := run(); code != 0 {
		t.Fatalf("next run exit code = %d, stderr=%q", code, stderr.String())
	}
	if _, exists := store.values["REMOVED_SECRET"]; exists {
		t.Fatal("next run did not delete a name removed from the source")
	}
	if _, exists := store.values["OLD_SECRET"]; exists {
		t.Fatal("next run did not delete OLD_SECRET, a name the state file recorded")
	}
	wantFinal := unionNames(secretMap.ExportedVarNames)
	if got, err := readState(statePath); err != nil || !reflect.DeepEqual(got, wantFinal) {
		t.Fatalf("state after successful run = %v, %v; want %v", got, err, wantFinal)
	}
}

func TestRunStateWriteFailureDoesNotChangeSecrets(t *testing.T) {
	localData := t.TempDir()
	store := newFakeEnvStore()
	store.values["REMOVED_SECRET"] = "old value"
	before := maps.Clone(store.values)
	runner := fixtureRunner()
	mapOutput, err := json.Marshal(fixtureSecretMap())
	if err != nil {
		t.Fatal(err)
	}
	runner.outputs[testMapPath] = mapOutput
	stateWrites := 0
	stateWriter := func(string, []string) error {
		stateWrites++
		return errors.New("state write failed")
	}
	var stdout, stderr bytes.Buffer
	getenv := func(name string) string {
		switch name {
		case "USERPROFILE":
			return `C:\Users\me`
		case "APPDATA":
			return `C:\Users\me\AppData\Roaming`
		case "LOCALAPPDATA":
			return localData
		default:
			return ""
		}
	}
	links := func(checkout, string, string) error { return nil }
	binaries := func(context.Context, Runner, EnvStore, checkout, string) error { return nil }
	code := runWithArgsAndPhasesAndStateWriter(context.Background(), getenv, &stdout, &stderr, testArgs(), runner.Runner(), func() (EnvStore, error) { return store, nil }, links, binaries, stateWriter)
	if code == 0 || stateWrites != 1 {
		t.Fatalf("run exit code = %d, state writes = %d; want failure at first state write", code, stateWrites)
	}
	if !reflect.DeepEqual(store.values, before) || len(store.sets) != 0 || len(store.deleted) != 0 || store.broadcasts != 0 {
		t.Fatalf("store changed when state write failed: values=%v sets=%v deletes=%v broadcasts=%d", store.values, store.sets, store.deleted, store.broadcasts)
	}
}

func TestRunSyncsSecretsAndManagedState(t *testing.T) {
	localData := t.TempDir()
	store := newFakeEnvStore()
	runner := fixtureRunner()
	links := func(checkout, string, string) error { return nil }
	binaries := func(context.Context, Runner, EnvStore, checkout, string) error { return nil }
	runner.outputs[testMapPath] = []byte(`{"team":[{"var":"WOODPECKER_TOKEN","key":"example-team-token"},{"var":"GITHUB_PERSONAL_ACCESS_TOKEN","key":"example-scoped-token","personalOnly":true}],"litellm":{"baseUrl":{"var":"LITELLM_BASE_URL","key":"example-base-url"},"apiKey":{"var":"LITELLM_API_KEY","key":"example-api-key"}},"linear":{"var":"LINEAR_API_KEY","key":"example-linear-key"},"search":[{"var":"EXA_API_KEY","key":"example-search-one"},{"var":"PARALLEL_API_KEY","key":"example-search-two"},{"var":"BRAVE_API_KEY","key":"example-search-three"}],"fileExports":[{"var":"DL_CLAIM_TOKEN_FILE","key":"example-file-token"}],"literals":{"WOODPECKER_SERVER":"https://ci.example.test","LINEAR_TEAM_ID":"EX"},"exportedVarNames":["WOODPECKER_TOKEN","GITHUB_PERSONAL_ACCESS_TOKEN","LITELLM_BASE_URL","LITELLM_API_KEY","LITELLM_MCP_URL","LINEAR_API_KEY","EXA_API_KEY","PARALLEL_API_KEY","BRAVE_API_KEY","DL_CLAIM_TOKEN_FILE","WOODPECKER_SERVER","LINEAR_TEAM_ID"]}`)
	var stdout, stderr bytes.Buffer
	getenv := func(name string) string {
		switch name {
		case "USERPROFILE":
			return `C:\Users\me`
		case "APPDATA":
			return `C:\Users\me\AppData\Roaming`
		case "LOCALAPPDATA":
			return localData
		default:
			return ""
		}
	}
	code := runWithArgsAndPhases(context.Background(), getenv, &stdout, &stderr, testArgs(), runner.Runner(), func() (EnvStore, error) {
		return store, nil
	}, links, binaries)
	if code != 0 {
		t.Fatalf("run exit code = %d, stderr=%q", code, stderr.String())
	}
	statePath := filepath.Join(localData, "rigel", "windows-agent-sync", "state.json")
	managed, err := readState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(managed) != len(fixtureSecretMap().ExportedVarNames) {
		t.Fatalf("managed count = %d", len(managed))
	}
	if store.values["LITELLM_MCP_URL"] != "https://llm.example/mcp/" {
		t.Fatalf("MCP env = %q", store.values["LITELLM_MCP_URL"])
	}

	tests := []struct {
		name   string
		mutate func(*SecretMap, *fakeRunner)
	}{
		{
			name: "missing key",
			mutate: func(_ *SecretMap, runner *fakeRunner) {
				runner.failAt = 2
			},
		},
		{
			name: "blank key",
			mutate: func(_ *SecretMap, runner *fakeRunner) {
				runner.outputs["/run/secrets/example-team-token"] = []byte(" \r\n")
			},
		},
		{
			name: "scoped Team key missing",
			mutate: func(_ *SecretMap, runner *fakeRunner) {
				delete(runner.outputs, "/run/secrets/example-scoped-token")
			},
		},
		{
			name: "half LiteLLM pair",
			mutate: func(_ *SecretMap, runner *fakeRunner) {
				delete(runner.outputs, "/run/secrets/example-api-key")
			},
		},
		{
			name: "export list mismatch",
			mutate: func(secretMap *SecretMap, _ *fakeRunner) {
				secretMap.ExportedVarNames = secretMap.ExportedVarNames[:len(secretMap.ExportedVarNames)-1]
			},
		},
		{
			name: "WSL failure",
			mutate: func(_ *SecretMap, runner *fakeRunner) {
				runner.failAt = 1
			},
		},
		{
			name:   "staging failure",
			mutate: func(_ *SecretMap, _ *fakeRunner) {},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runFailureCase(t, test.name, test.mutate)
		})
	}
}

func runFailureCase(t *testing.T, name string, mutate func(*SecretMap, *fakeRunner)) {
	t.Helper()
	localData := t.TempDir()
	fileDir := filepath.Join(localData, "rigel", "secrets")
	if name == "staging failure" {
		if err := os.MkdirAll(filepath.Dir(fileDir), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fileDir, []byte("block"), 0o600); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := os.MkdirAll(fileDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fileDir, "dl-claim-token"), []byte("old file"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store := newFakeEnvStore()
	store.values["KEEP"] = "old value"
	store.values["DL_CLAIM_TOKEN_FILE"] = filepath.Join(fileDir, "dl-claim-token")
	before := make(map[string]string, len(store.values))
	maps.Copy(before, store.values)
	runner := fixtureRunner()
	secretMap := fixtureSecretMap()
	mutate(&secretMap, runner)
	mapOutput, err := json.Marshal(secretMap)
	if err != nil {
		t.Fatal(err)
	}
	runner.outputs[testMapPath] = mapOutput
	var stdout, stderr bytes.Buffer
	getenv := func(name string) string {
		switch name {
		case "USERPROFILE":
			return `C:\Users\me`
		case "APPDATA":
			return `C:\Users\me\AppData\Roaming`
		case "LOCALAPPDATA":
			return localData
		default:
			return ""
		}
	}
	links := func(checkout, string, string) error { return nil }
	binaries := func(context.Context, Runner, EnvStore, checkout, string) error { return nil }
	code := runWithArgsAndPhases(context.Background(), getenv, &stdout, &stderr, testArgs(), runner.Runner(), func() (EnvStore, error) {
		return store, nil
	}, links, binaries)
	if code == 0 {
		t.Fatal("run succeeded despite a secret synchronization failure")
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !reflect.DeepEqual(store.values, before) || len(store.sets) != 0 || len(store.deleted) != 0 || store.broadcasts != 0 {
		t.Fatalf("store changed on failed run: values=%v sets=%v deletes=%v broadcasts=%d", store.values, store.sets, store.deleted, store.broadcasts)
	}
	statePath := filepath.Join(localData, "rigel", "windows-agent-sync", "state.json")
	if name == "staging failure" {
		// Staging runs after the state pre-write, so the full export union is already recorded.
		if got, err := readState(statePath); err != nil || !reflect.DeepEqual(got, unionNames(fixtureSecretMap().ExportedVarNames)) {
			t.Fatalf("staging failure state = %v, %v; want the pre-written export union", got, err)
		}
	} else if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s wrote state.json before resolve finished: %v", name, err)
	}
	for _, value := range runner.outputs {
		secret := strings.TrimSpace(string(value))
		if secret != "" && strings.Contains(stderr.String(), secret) {
			t.Fatal("error output contains secret value")
		}
	}
	if name == "staging failure" {
		contents, err := os.ReadFile(fileDir)
		if err != nil || string(contents) != "block" {
			t.Fatalf("staging blocker changed: contents=%q err=%v", contents, err)
		}
	} else {
		contents, err := os.ReadFile(filepath.Join(fileDir, "dl-claim-token"))
		if err != nil || string(contents) != "old file" {
			t.Fatalf("old export changed: contents=%q err=%v", contents, err)
		}
	}
}

func runWithArgs(ctx context.Context, getenv environment, stdout, stderr *bytes.Buffer, args []string, runner Runner, createStore storeFactory) int {
	if runner == nil {
		runner = execRunner
	}
	if createStore == nil {
		createStore = func() (EnvStore, error) { return nil, errors.New("unexpected store creation") }
	}
	command := newRootCommand(ctx, getenv, stdout, stderr, runner, createStore)
	command.SetArgs(args)
	if err := command.Execute(); err != nil {
		slog.New(slog.NewTextHandler(stderr, nil)).Error("windows-agent-sync failed", "error", err)
		return 1
	}
	return 0
}

func runWithArgsAndPhases(ctx context.Context, getenv environment, stdout, stderr *bytes.Buffer, args []string, runner Runner, createStore storeFactory, links func(checkout, string, string) error, binaries func(context.Context, Runner, EnvStore, checkout, string) error) int {
	return runWithArgsAndPhasesAndStateWriter(ctx, getenv, stdout, stderr, args, runner, createStore, links, binaries, writeState)
}

func runWithArgsAndPhasesAndStateWriter(ctx context.Context, getenv environment, stdout, stderr *bytes.Buffer, args []string, runner Runner, createStore storeFactory, links func(checkout, string, string) error, binaries func(context.Context, Runner, EnvStore, checkout, string) error, stateWriter func(string, []string) error) int {
	command := newRootCommandWithStateWriter(ctx, getenv, stdout, stderr, runner, createStore, links, binaries, stateWriter)
	command.SetArgs(args)
	if err := command.Execute(); err != nil {
		slog.New(slog.NewTextHandler(stderr, nil)).Error("windows-agent-sync failed", "error", err)
		return 1
	}
	return 0
}

func TestReadWriteState(t *testing.T) {
	tests := []struct {
		name    string
		initial string
		want    []string
		wantErr bool
	}{
		{name: "missing state", want: []string{}},
		{name: "managed entries", initial: `{"managed":["A","B"]}`, want: []string{"A", "B"}},
		{name: "malformed state", initial: "not json", wantErr: true},
		{name: "invalid managed name", initial: `{"managed":["A","bad=name"]}`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if test.initial != "" {
				if err := os.WriteFile(path, []byte(test.initial), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := readState(path)
			if (err != nil) != test.wantErr {
				t.Fatalf("readState() error = %v, wantErr %v", err, test.wantErr)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("readState() = %v, want %v", got, test.want)
			}
		})
	}

	path := filepath.Join(t.TempDir(), "nested", "state.json")
	if err := writeState(path, []string{"A", "B"}); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeState(path, []string{"B", "C"}); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("writeState left changed state unchanged")
	}
	var saved state
	if err := json.Unmarshal(second, &saved); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(saved.Managed, []string{"B", "C"}) {
		t.Fatalf("saved state = %v, want [B C]", saved.Managed)
	}
}

func TestWriteStateReplacementFailurePreservesPreviousState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := writeState(path, []string{"A"}); err != nil {
		t.Fatal(err)
	}
	priorInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	prior, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writeErr := writeStateWithReplace(path, []string{"B"}, func(string, string) error {
		return errors.New("replacement failed")
	})
	if writeErr == nil || !strings.Contains(writeErr.Error(), "committing state file") {
		t.Fatalf("writeStateWithReplace() error = %v, want replacement failure", writeErr)
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(current, prior) {
		t.Fatalf("state after replacement failure = %q, %v; want prior %q", current, err, prior)
	}
	currentInfo, err := os.Stat(path)
	if err != nil || !os.SameFile(priorInfo, currentInfo) {
		t.Fatalf("state file identity changed after replacement failure: info=%v err=%v", currentInfo, err)
	}
	temps, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".state-*.tmp"))
	if err != nil || len(temps) != 0 {
		t.Fatalf("staged state files after replacement failure: %v, %v", temps, err)
	}
}

func TestResolveRunnerFailureDoesNotExposeValue(t *testing.T) {
	runner := fixtureRunner()
	runner.failAt = 8
	vars, files, err := resolve(context.Background(), runner.Runner(), "NixOS", fixtureSecretMap())
	if err == nil || !strings.Contains(err.Error(), "example-base-url") {
		t.Fatalf("resolve() error = %v", err)
	}
	if len(vars) != 0 || len(files) != 0 {
		t.Fatalf("resolve() returned partial secrets: %v %v", keys(vars), keys(files))
	}
	for _, secret := range runner.outputs {
		if strings.Contains(err.Error(), strings.TrimSpace(string(secret))) {
			t.Fatal("error contains a secret value")
		}
	}
}

func TestSyncPhaseOrder(t *testing.T) {
	t.Run("links failure stops before secrets", func(t *testing.T) {
		var called []string
		wantErr := errors.New("links failed")
		err := runSyncPhases(
			func() error { called = append(called, "links"); return wantErr },
			func() error { called = append(called, "binaries"); return nil },
			func() error { called = append(called, "secrets"); return nil },
		)
		if !errors.Is(err, wantErr) {
			t.Fatalf("runSyncPhases() error = %v, want wrapped links error", err)
		}
		if !reflect.DeepEqual(called, []string{"links"}) {
			t.Fatalf("phases called = %v", called)
		}
	})
	t.Run("binary failure stops before secrets", func(t *testing.T) {
		var called []string
		wantErr := errors.New("binaries failed")
		err := runSyncPhases(
			func() error { called = append(called, "links"); return nil },
			func() error { called = append(called, "binaries"); return wantErr },
			func() error { called = append(called, "secrets"); return nil },
		)
		if !errors.Is(err, wantErr) || !reflect.DeepEqual(called, []string{"links", "binaries"}) {
			t.Fatalf("phases = %v, error = %v", called, err)
		}
	})
	var called []string
	err := runSyncPhases(
		func() error { called = append(called, "links"); return nil },
		func() error { called = append(called, "binaries"); return nil },
		func() error { called = append(called, "secrets"); return nil },
	)
	if err != nil || !reflect.DeepEqual(called, []string{"links", "binaries", "secrets"}) {
		t.Fatalf("phase order = %v, error = %v", called, err)
	}
}

func TestRunAllPhasesSharesOneStore(t *testing.T) {
	for _, test := range []struct {
		name        string
		linksErr    error
		binariesErr error
		wantOpens   int
		wantSecrets bool
	}{
		{name: "links failure opens no store", linksErr: errors.New("links failed")},
		{name: "binaries failure skips secrets", binariesErr: errors.New("binaries failed"), wantOpens: 1},
		{name: "secrets reuse the binaries store", wantOpens: 1, wantSecrets: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &closeableFakeEnvStore{fakeEnvStore: newFakeEnvStore()}
			opens := 0
			createStore := func() (EnvStore, error) { opens++; return store, nil }
			runner := fixtureRunner()
			mapOutput, err := json.Marshal(fixtureSecretMap())
			if err != nil {
				t.Fatal(err)
			}
			runner.outputs[testMapPath] = mapOutput
			var binariesStore EnvStore
			links := func(checkout, string, string) error { return test.linksErr }
			binaries := func(_ context.Context, _ Runner, got EnvStore, _ checkout, _ string) error {
				binariesStore = got
				return test.binariesErr
			}
			err = runAllPhases(context.Background(), runner.Runner(), createStore, testCheckout(t), `C:\Users\me`, `C:\Users\me\AppData\Roaming`, t.TempDir(), links, binaries, writeState)
			if (err == nil) != test.wantSecrets {
				t.Fatalf("runAllPhases() error = %v", err)
			}
			if opens != test.wantOpens || store.closed != (test.wantOpens == 1) {
				t.Fatalf("store opens = %d, closed = %v", opens, store.closed)
			}
			if test.wantOpens == 1 && binariesStore != store {
				t.Fatal("binaries phase did not receive the opened store")
			}
			if ran := len(runner.calls) > 0; ran != test.wantSecrets {
				t.Fatalf("secrets phase ran = %v, want %v", ran, test.wantSecrets)
			}
			if !test.wantSecrets && len(store.sets) != 0 {
				t.Fatalf("secrets phase wrote values after an earlier failure: %v", store.sets)
			}
			if test.wantSecrets && store.values["LITELLM_MCP_URL"] == "" {
				t.Fatal("secrets phase did not write to the shared store")
			}
		})
	}
}

func TestRunAllPhasesRunsLinksAndBinariesWhenSecretMapFails(t *testing.T) {
	store := newFakeEnvStore()
	runner := fixtureRunner()
	runner.failAt = 1 // the first runner call is the WSL nix eval of the secret map
	localData := t.TempDir()
	var phases []string
	links := func(checkout, string, string) error {
		phases = append(phases, "links")
		return nil
	}
	binaries := func(context.Context, Runner, EnvStore, checkout, string) error {
		phases = append(phases, "binaries")
		return nil
	}
	createStore := func() (EnvStore, error) { return store, nil }
	err := runAllPhases(context.Background(), runner.Runner(), createStore, testCheckout(t), `C:\Users\me`, `C:\Users\me\AppData\Roaming`, localData, links, binaries, writeState)
	if err == nil {
		t.Fatal("runAllPhases() succeeded despite a failed secret map evaluation")
	}
	if !reflect.DeepEqual(phases, []string{"links", "binaries"}) {
		t.Fatalf("phases before secrets = %v, want links then binaries", phases)
	}
	if len(store.sets) != 0 || len(store.deleted) != 0 || store.broadcasts != 0 {
		t.Fatalf("secrets changed the store: sets=%v deletes=%v broadcasts=%d", store.sets, store.deleted, store.broadcasts)
	}
	statePath := filepath.Join(localData, "rigel", "windows-agent-sync", "state.json")
	if _, statErr := os.Stat(statePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("state file exists after failed map evaluation: %v", statErr)
	}
}
