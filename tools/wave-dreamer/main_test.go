package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	dreamerClonePath = "/srv/example/clone"
	testRunsDir      = "/srv/example/runs"
	testRemote       = "https://git.example.org/example/repo.git"
)

const validConfig = "  autolearn.journalDir = /srv/example/lessons\n"

type fakeInfo struct{}

func (fakeInfo) Name() string       { return "dreamer-clone" }
func (fakeInfo) Size() int64        { return 0 }
func (fakeInfo) Mode() os.FileMode  { return 0o700 }
func (fakeInfo) ModTime() time.Time { return time.Time{} }
func (fakeInfo) IsDir() bool        { return true }
func (fakeInfo) Sys() any           { return nil }

type fakeFileSystem struct {
	paths []string
	globs []string
	moves [][2]string
}

func (fs *fakeFileSystem) Stat(path string) (os.FileInfo, error) {
	if slices.Contains(fs.paths, path) {
		return fakeInfo{}, nil
	}
	return nil, os.ErrNotExist
}
func (fs *fakeFileSystem) Glob(string) ([]string, error)   { return fs.globs, nil }
func (*fakeFileSystem) MkdirAll(string, os.FileMode) error { return nil }
func (fs *fakeFileSystem) Rename(oldPath, newPath string) error {
	fs.moves = append(fs.moves, [2]string{oldPath, newPath})
	fs.paths = append(fs.paths, newPath)
	fs.paths = slices.DeleteFunc(fs.paths, func(path string) bool { return path == oldPath })
	return nil
}

type dreamerCase struct {
	name           string
	doneExists     bool
	configFails    bool
	missingJournal bool
	cloneExists    bool
	cloneValid     bool
	brokenExists   bool
	wantError      string
	wantClone      bool
	wantMoved      bool
}

func TestRunBranches(t *testing.T) {
	cases := []dreamerCase{
		{name: "done exists", doneExists: true},
		{name: "config list fails", configFails: true, wantError: "omp config list failed"},
		{name: "journalDir missing", missingJournal: true, wantError: "no autolearn.journalDir"},
		{name: "clone ok", cloneExists: true, cloneValid: true},
		{name: "clone missing", wantClone: true},
		{name: "broken clone", cloneExists: true, wantClone: true, wantMoved: true},
		{name: "second broken clone", cloneExists: true, brokenExists: true, wantError: "already exists; fix or remove it"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) { runCase(t, test) })
	}
}

func runCase(t *testing.T, test dreamerCase) {
	t.Helper()
	fs := &fakeFileSystem{}
	if test.doneExists {
		fs.paths = append(fs.paths, filepath.Join(testRunsDir, "2026-11-12.done"))
	}
	if test.cloneExists {
		fs.paths = append(fs.paths, dreamerClonePath)
	}
	if test.brokenExists {
		fs.globs = []string{dreamerClonePath + ".broken-previous"}
	}
	env := testEnvironment()
	var commands []command
	deps := dependencies{
		fs:     fs,
		getenv: func(key string) string { return env[key] },
		execute: func(_ context.Context, request command) ([]byte, error) {
			commands = append(commands, request)
			return fakeExecute(test, request)
		},
	}
	err := run(context.Background(), deps)
	if test.wantError == "" && err != nil {
		t.Fatalf("run() error = %v, want nil", err)
	}
	if test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
		t.Fatalf("run() error = %v, want containing %q", err, test.wantError)
	}
	if test.wantClone != commandSeen(commands, "clone") {
		t.Errorf("clone command seen = %t, want %t", commandSeen(commands, "clone"), test.wantClone)
	}
	if test.wantMoved != (len(fs.moves) == 1) {
		t.Errorf("moved clone = %t, want %t", len(fs.moves) == 1, test.wantMoved)
	}
	if test.doneExists && len(commands) != 1 {
		t.Errorf("done short-circuit ran %d commands, want only date", len(commands))
	}
	if (test.configFails || test.missingJournal) && commandSeen(commands, "remote") {
		t.Error("clone validation ran after a preflight failure")
	}
	if err == nil && !test.doneExists {
		assertDreamerInvocation(t, test, commands)
	}
}

func fakeExecute(test dreamerCase, request command) ([]byte, error) {
	if request.name == "/date" {
		switch request.args[1] {
		case "+%F":
			return []byte("2026-11-12\n"), nil
		case "+%H%M%S":
			return []byte("040506\n"), nil
		case "+%Y%m%dT%H%M%SZ":
			return []byte("20261112T040506Z\n"), nil
		}
	}
	if request.name == "/bridge" {
		return []byte("SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt\x00LITELLM_BASE_URL=https://llm.example/v1\x00LITELLM_API_KEY=key-value\x00"), nil
	}
	if request.name == "/omp" && slices.Equal(request.args, []string{"--config", "/config.yml", "config", "list"}) {
		if test.configFails {
			return nil, errors.New("config unavailable")
		}
		if test.missingJournal {
			return []byte("autolearn.enabled = true\n"), nil
		}
		return []byte(validConfig), nil
	}
	if request.name == "/jj" {
		if slices.Contains(request.args, "remote") {
			if !test.cloneValid {
				return nil, errors.New("not a clone")
			}
			return []byte("origin " + testRemote + "\n"), nil
		}
		if slices.Contains(request.args, "clone") || slices.Contains(request.args, "fetch") || slices.Contains(request.args, "new") || slices.Contains(request.args, "log") {
			return nil, nil
		}
	}
	if request.name == "/omp" && len(request.args) > 0 && request.args[0] == "-p" {
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected command: %s %v", request.name, request.args)
}

func testEnvironment() map[string]string {
	return map[string]string{
		"WAVE_DREAMER_DATE": "/date", "WAVE_DREAMER_BRIDGE": "/bridge", "WAVE_DREAMER_OMP": "/omp",
		"WAVE_DREAMER_JJ": "/jj", "WAVE_DREAMER_OMP_PATH": "/nix/tools", "WAVE_DREAMER_CONFIG": "/config.yml",
		"WAVE_DREAMER_PROMPT": "/dream.md", "WAVE_DREAMER_MAX_TIME": "30m",
		"WAVE_DREAMER_CLONE": dreamerClonePath, "WAVE_DREAMER_REMOTE": testRemote, "WAVE_DREAMER_RUNS_DIR": testRunsDir,
		"HOME": "/srv/example", "XDG_CONFIG_HOME": "/srv/example/.config",
		"XDG_DATA_HOME": "/srv/example/.local/share", "HINDSIGHT_URL": "https://memory.example",
		"XDG_RUNTIME_DIR": "/run/user/1000", "DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus",
		"LITELLM_BASE_URL": "inherited-must-not-be-used", "LITELLM_API_KEY": "inherited-must-not-be-used",
		"SSL_CERT_FILE": "inherited-must-not-be-used", "GH_TOKEN": "must-not-pass", "GIT_ASKPASS": "must-not-pass",
	}
}

func commandSeen(commands []command, parts ...string) bool {
	for _, request := range commands {
		if request.name != "/jj" {
			continue
		}
		found := true
		for _, part := range parts {
			if !slices.Contains(request.args, part) {
				found = false
				break
			}
		}
		if found {
			return true
		}
	}
	return false
}

func assertDreamerInvocation(t *testing.T, test dreamerCase, commands []command) {
	t.Helper()
	fetch, start := -1, -1
	for index, request := range commands {
		if request.name == "/jj" && slices.Contains(request.args, "fetch") {
			fetch = index
		}
		if request.name == "/omp" && len(request.args) > 0 && request.args[0] == "-p" {
			start = index
		}
	}
	if fetch < 0 || start < 0 || fetch >= start {
		t.Fatalf("fetch index = %d, dreamer index = %d; fetch must precede omp", fetch, start)
	}
	if test.cloneValid && commandSeen(commands, "clone") {
		t.Error("valid clone was cloned again")
	}
	for _, request := range commands {
		if request.name != "/omp" || len(request.args) == 0 || request.args[0] != "-p" {
			continue
		}
		if request.dir != dreamerClonePath {
			t.Errorf("omp cwd = %q, want %q", request.dir, dreamerClonePath)
		}
		wantArgs := []string{
			"-p", "--max-time", "30m", "--session-id", "dreamer-2026-11-12-040506",
			"--model", "@dream", "--tools", "read,bash,manage_skill", "--config", "/config.yml", "@/dream.md",
			"Run dreamer-2026-11-12-040506 for day 2026-11-12. Write the done file as 2026-11-12.done.",
		}
		if !slices.Equal(request.args, wantArgs) {
			t.Errorf("omp args = %q, want %q", request.args, wantArgs)
		}
		wantEnv := []string{
			"PATH=/nix/tools", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=",
			"HOME=/srv/example", "XDG_CONFIG_HOME=/srv/example/.config",
			"XDG_DATA_HOME=/srv/example/.local/share", "XDG_RUNTIME_DIR=/run/user/1000",
			"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus", "LITELLM_BASE_URL=https://llm.example/v1",
			"LITELLM_API_KEY=key-value", "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt",
			"HINDSIGHT_URL=https://memory.example",
		}
		if !slices.Equal(request.env, wantEnv) {
			t.Errorf("omp environment = %q, want %q", request.env, wantEnv)
		}
	}
}

func TestParseBridgeEnvironmentRejectsUnexpectedEntries(t *testing.T) {
	for _, bridgeOutput := range [][]byte{
		[]byte("EXAMPLE_KEY_FILE=/srv/example/key\x00"),
		[]byte("LITELLM_API_KEY=a\x00LITELLM_API_KEY=b\x00"),
		[]byte("not-an-assignment\x00"),
	} {
		if _, err := parseBridgeEnvironment(bridgeOutput); err == nil {
			t.Errorf("parseBridgeEnvironment(%q) succeeded, want error", bridgeOutput)
		}
	}
}

func TestOMPEnvironmentOmitsHindsightWhenUnset(t *testing.T) {
	deps := dependencies{
		getenv: func(key string) string {
			if key == "WAVE_DREAMER_OMP_PATH" {
				return "/nix/tools"
			}
			return ""
		},
	}
	env, err := deps.ompEnvironment([]byte("SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt\x00"))
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(env, func(value string) bool { return strings.HasPrefix(value, "HINDSIGHT_URL=") }) {
		t.Fatalf("environment includes an unset Hindsight URL: %q", env)
	}
}
