// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"errors"
	"strings"
	"testing"
)

func TestServiceURLsLongHelp(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "root command", args: []string{"--help"}},
		{name: "coverage command", args: []string{"coverage", "--help"}},
		{name: "repository inventory command", args: []string{"repo-inventory", "--help"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout strings.Builder
			var stderr strings.Builder
			command := newRootCommand(t.Context(), func(string) string { return "" }, &stdout, &stderr)
			command.SetArgs(test.args)
			if err := command.Execute(); err != nil {
				t.Fatal(err)
			}
			output := stdout.String()
			if !strings.Contains(output, "Exit codes:") {
				t.Fatalf("help output omits exit codes: %q", output)
			}
			if test.name != "coverage command" && (!strings.Contains(output, "REPO_INVENTORY_APP_PRIVATE_KEY") || !strings.Contains(output, "REPO_INVENTORY_APP_CLIENT_ID") || !strings.Contains(output, "REPO")) {
				t.Fatalf("help output omits repository inventory environment: %q", output)
			}
			if test.name == "coverage command" && strings.Contains(output, "REPO_INVENTORY_APP_PRIVATE_KEY") {
				t.Fatalf("coverage help unexpectedly lists repository inventory environment: %q", output)
			}
			if stderr.Len() != 0 {
				t.Fatalf("help wrote stderr: %q", stderr.String())
			}
		})
	}
}

func TestMissingConfigFlagExitsOperationalError(t *testing.T) {
	for _, args := range [][]string{{}, {"coverage"}, {"repo-inventory"}} {
		t.Run(strings.Join(append([]string{"root"}, args...), " "), func(t *testing.T) {
			var stdout strings.Builder
			var stderr strings.Builder
			code := Main(t.Context(), args, func(string) string { return "" }, &stdout, &stderr)
			if code != 2 {
				t.Fatalf("Main() = %d, want 2", code)
			}
			if !strings.Contains(stderr.String(), "required flag(s)") {
				t.Fatalf("stderr = %q, want missing config flag error", stderr.String())
			}
		})
	}
}

func TestWriteFailureReturnsOperationalErrorStatus(t *testing.T) {
	if code := writeFailure(errorWriter{}, "operational failure"); code != 2 {
		t.Fatalf("writeFailure() = %d, want 2", code)
	}
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}
