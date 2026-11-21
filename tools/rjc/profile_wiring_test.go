package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// exampleDevenvNixpkgs is a generic devenv nixpkgs reference for hook-script tests.
const exampleDevenvNixpkgs = "github:example/devenv-nixpkgs/1111111111111111111111111111111111111111"

// TestRunRefusesWithoutProfiles checks that a run with no --profiles and no RJC_PROFILES exits 2 at the profiles gate.
func TestRunRefusesWithoutProfiles(t *testing.T) {
	t.Setenv(profilesEnv, "")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"-o", "json", "status"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit = %d, want 2; stderr=%s", code, &stderr)
	}
	var got struct {
		Gate string `json:"gate"`
	}
	if err := json.Unmarshal(stderr.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", stderr.String(), err)
	}
	if got.Gate != "profiles" {
		t.Fatalf("gate = %q, want profiles", got.Gate)
	}
}

// TestProfilesFlagWinsOverEnv checks that --profiles takes precedence over RJC_PROFILES, and the env is the fallback.
func TestProfilesFlagWinsOverEnv(t *testing.T) {
	t.Setenv(profilesEnv, "from-env.cue")
	if got, err := resolveProfilesPath("from-flag.cue"); err != nil || got != "from-flag.cue" {
		t.Fatalf("flag and env: got %q err=%v, want from-flag.cue", got, err)
	}
	if got, err := resolveProfilesPath(""); err != nil || got != "from-env.cue" {
		t.Fatalf("env only: got %q err=%v, want from-env.cue", got, err)
	}
}

// TestSchemaRejectsRepoWithoutRequiredFields checks that a repo entry missing issuePrefix or specDirs fails validation.
func TestSchemaRejectsRepoWithoutRequiredFields(t *testing.T) {
	data, err := os.ReadFile("testdata/profile.cue")
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"issuePrefix", "specDirs"} {
		t.Run(field, func(t *testing.T) {
			var kept []string
			inDocs := false
			for line := range strings.SplitSeq(string(data), "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, `"example/docs"`) {
					inDocs = true
				}
				if inDocs && strings.HasPrefix(trimmed, field+":") {
					continue
				}
				if inDocs && trimmed == "}" {
					inDocs = false
				}
				kept = append(kept, line)
			}
			path := filepath.Join(t.TempDir(), "profile.cue")
			writeTestFile(t, path, strings.Join(kept, "\n"))
			if _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), field) {
				t.Fatalf("loadConfig without %s = %v, want a validation error naming it", field, err)
			}
		})
	}
}

// TestFastHookPolicyUsesGateRunnerDir checks that the local fast-hook policy names the profile's gate-runner directory.
func TestFastHookPolicyUsesGateRunnerDir(t *testing.T) {
	data, err := os.ReadFile("testdata/hk.pkl")
	if err != nil {
		t.Fatal(err)
	}
	// Drop the leading comment line: the policy matches the config with comments stripped.
	_, body, _ := strings.Cut(string(data), "\n")
	data = []byte(body)
	custom := strings.ReplaceAll(body, "./tools/gate-runner", "./tools/custom-gate")
	if !fastHookRE("tools/custom-gate").MatchString(custom) {
		t.Fatal("fast hook policy does not accept the configured gate-runner directory")
	}
	if fastHookRE("tools/custom-gate").MatchString(string(data)) {
		t.Fatal("fast hook policy accepts a gate-runner directory the profile does not name")
	}
}

// TestRunRootChecksUsesRootChecksDir checks that the real root-checks command runs the profile's directory.
func TestRunRootChecksUsesRootChecksDir(t *testing.T) {
	f := fixtureRepo(t)
	f.repo.profile.RootChecksDir = "tools/custom-checks"
	bin := t.TempDir()
	args := filepath.Join(t.TempDir(), "args")
	writeFakeTool(t, filepath.Join(bin, "go"), "#!/bin/sh\nprintf '%s' \"$*\" > "+shQuote(args)+"\n")
	f.repo.env = append(f.repo.env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := f.repo.runRootChecks(t.Context(), t.TempDir(), []string{"biome"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "run ./tools/custom-checks --checks biome" {
		t.Fatalf("go argv = %q", got)
	}
}

// TestSSHHookScriptUsesDevenvNixpkgs checks that the remote hook script starts devenv from the profile's reference,
// and that ProfileFor carries the value from the profile file.
func TestSSHHookScriptUsesDevenvNixpkgs(t *testing.T) {
	cfg := loadExampleConfig(t)
	cfg.DevenvNixpkgs = exampleDevenvNixpkgs
	p, err := ProfileFor(cfg, "https://github.com/example/app.git")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sshHookScript(p.DevenvNixpkgs), exampleDevenvNixpkgs+"#devenv") {
		t.Fatalf("hook script does not start devenv from %q", exampleDevenvNixpkgs)
	}
}
