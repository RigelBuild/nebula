package main

import (
	"bytes"
	"testing"
)

// TestRunHelpPrintsUsageWithoutKeychain: --help prints the usage line to stdout with
// exit 0, and reads no keychain name, so it works with the required env unset.
func TestRunHelpPrintsUsageWithoutKeychain(t *testing.T) {
	for _, arg := range []string{"--help", "-h"} {
		t.Run(arg, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(t.Context(), []string{arg}, func(string) string { return "" }, &stdout, &stderr)
			if code != 0 {
				t.Errorf("exit code = %d; want 0", code)
			}
			const want = "Usage: akiflow login | akiflow tools | akiflow add <title...>\n"
			if got := stdout.String(); got != want {
				t.Errorf("stdout = %q; want %q", got, want)
			}
			if stderr.Len() != 0 {
				t.Errorf("stderr = %q; want empty", stderr.String())
			}
		})
	}
}

// testEnv supplies fixture keychain names for the required env vars. Every case
// here fails before any Keychain access, so these values never reach the Keychain.
func testEnv(key string) string {
	switch key {
	case keychainServiceEnv:
		return "akiflow-test"
	case keychainAccountEnv:
		return "akiflow-test-account"
	}
	return ""
}

// TestRunExitCodesAndStderr drives run with argv that fails before any Keychain or
// network use, and checks the exit code and the exact stderr bytes.
func TestRunExitCodesAndStderr(t *testing.T) {
	const usage = "Usage: akiflow login | akiflow tools | akiflow add <title...>\n"
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantErr  string
	}{
		{name: "no arguments prints usage", args: []string{}, wantCode: 1, wantErr: usage},
		{name: "unknown command prints usage", args: []string{"bogus"}, wantCode: 1, wantErr: usage},
		{name: "login with an extra argument prints usage", args: []string{"login", "x"}, wantCode: 1, wantErr: usage},
		{name: "blank add title is rejected", args: []string{"add", " "}, wantCode: 1, wantErr: "Task title must not be empty\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(t.Context(), tt.args, testEnv, &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("exit code = %d; want %d", code, tt.wantCode)
			}
			if got := stderr.String(); got != tt.wantErr {
				t.Errorf("stderr = %q; want %q", got, tt.wantErr)
			}
		})
	}
}

// TestRunRejectsBadKeychainService: a service name with whitespace fails before any
// Keychain access, and the exact error reaches stderr with exit code 1.
func TestRunRejectsBadKeychainService(t *testing.T) {
	badService := func(key string) string {
		if key == keychainServiceEnv {
			return "bad service"
		}
		return testEnv(key)
	}
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"login"}, badService, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d; want 1", code)
	}
	const want = "Invalid Keychain service: whitespace and quotes are not allowed\n"
	if got := stderr.String(); got != want {
		t.Errorf("stderr = %q; want %q", got, want)
	}
}

// TestRunRequiresKeychainNames: a missing service or account fails before any Keychain
// access, and stderr names the variable that must be set.
func TestRunRequiresKeychainNames(t *testing.T) {
	tests := []struct {
		name    string
		unset   string
		wantErr string
	}{
		{name: "missing service", unset: keychainServiceEnv, wantErr: "Keychain service is required; set AKIFLOW_KEYCHAIN_SERVICE\n"},
		{name: "missing account", unset: keychainAccountEnv, wantErr: "Keychain account is required; set AKIFLOW_KEYCHAIN_ACCOUNT\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(key string) string {
				if key == tt.unset {
					return ""
				}
				return testEnv(key)
			}
			var stdout, stderr bytes.Buffer
			code := run(t.Context(), []string{"login"}, getenv, &stdout, &stderr)
			if code != 1 {
				t.Errorf("exit code = %d; want 1", code)
			}
			if got := stderr.String(); got != tt.wantErr {
				t.Errorf("stderr = %q; want %q", got, tt.wantErr)
			}
		})
	}
}
