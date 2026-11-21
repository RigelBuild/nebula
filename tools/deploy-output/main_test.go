package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runWithOutput runs deploy-output with CI_STEP_OUTPUT set to output, or unset
// when output is empty. It returns the exit code and the captured stderr.
func runWithOutput(t *testing.T, output string, args ...string) (int, string) {
	t.Helper()
	getenv := func(key string) string {
		if key == "CI_STEP_OUTPUT" {
			return output
		}
		return ""
	}
	var stderr bytes.Buffer
	code := run(args, getenv, &bytes.Buffer{}, &stderr)
	return code, stderr.String()
}

func TestRunRecordsTargetStateAndExitCode(t *testing.T) {
	for _, tc := range []struct {
		name      string
		script    string
		code      int
		completed bool
	}{
		{name: "success", script: "exit 0", code: 0, completed: true},
		{name: "failure", script: "exit 7", code: 7},
		{name: "signal", script: "kill -TERM $$", code: 143},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "output")
			code, _ := runWithOutput(t, output, "--target", "aws:up", "--", "sh", "-c", tc.script)
			if code != tc.code {
				t.Fatalf("exit = %d, want %d", code, tc.code)
			}
			data, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			want := "deploy.target=aws:up\ndeploy.state=started\n"
			if tc.completed {
				want += "deploy.state=completed\n"
			}
			if string(data) != want {
				t.Fatalf("output = %q, want %q", data, want)
			}
		})
	}
}

func TestRunWithoutOutputPathKeepsExitCodes(t *testing.T) {
	if code, _ := runWithOutput(t, "", "--target", "a:b", "--", "sh", "-c", "exit 0"); code != 0 {
		t.Fatalf("success exit = %d, want 0", code)
	}
	if code, _ := runWithOutput(t, "", "--target", "a:b", "--", "/nonexistent-deploy-command"); code != spawnFailed {
		t.Fatalf("spawn exit = %d, want %d", code, spawnFailed)
	}
}

func TestRunSpawnFailureLeavesStartedOutput(t *testing.T) {
	output := filepath.Join(t.TempDir(), "output")
	code, stderr := runWithOutput(t, output, "--target", "aws:up", "--", "/nonexistent-deploy-command")
	if code != spawnFailed {
		t.Fatalf("exit = %d, want %d", code, spawnFailed)
	}
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "deploy-output: spawn failed:") {
		t.Fatalf("stderr = %q, want one spawn-failed line", stderr)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "deploy.target=aws:up\ndeploy.state=started\n" {
		t.Fatalf("output = %q", data)
	}
}

func TestRunUsageErrorsExitTwoAndWriteNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "no args", args: nil},
		{name: "no separator", args: []string{"--target", "a:b", "true"}},
		{name: "empty target", args: []string{"--target", "", "--", "true"}},
		{name: "wrong flag", args: []string{"--tgt", "a:b", "--", "true"}},
		{name: "no command", args: []string{"--target", "a:b", "--"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "output")
			code, stderr := runWithOutput(t, output, tc.args...)
			if code != usageError {
				t.Fatalf("exit = %d, want %d", code, usageError)
			}
			if !strings.Contains(stderr, usage) {
				t.Fatalf("stderr = %q, want usage", stderr)
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatalf("output file exists after a usage error: %v", err)
			}
		})
	}
}
