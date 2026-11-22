package keychain

import (
	"context"
	"encoding/hex"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
)

type recordingRunner struct {
	argv   []string
	stdin  string
	result CommandResult
	err    error
}

func (r *recordingRunner) run(_ context.Context, argv []string, stdin string) (CommandResult, error) {
	r.argv = argv
	r.stdin = stdin
	return r.result, r.err
}

func TestDecodeKeychainValue(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "decodes hex-encoded UTF-8 JSON", input: "7b2261223a22622063227d", want: `{"a":"b c"}`},
		{name: "preserves plain JSON output", input: "{\"a\":\"b c\"}\n", want: `{"a":"b c"}`},
		{name: "leaves non-JSON hex output unchanged", input: "deadbeef", want: "deadbeef"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decodeKeychainValue(tt.input); got != tt.want {
				t.Errorf("decodeKeychainValue(%q) = %q; want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestKeychainWritesPassCredentialThroughStdin(t *testing.T) {
	token := `{"access_token":"secret token"}`
	runner := &recordingRunner{}
	store, err := NewKeychainStore("akiflow-test", "akiflow-test-account", runner.run)
	if err != nil {
		t.Fatalf("NewKeychainStore: %v", err)
	}
	if err := store.Set(t.Context(), token); err != nil {
		t.Fatalf("set: %v", err)
	}
	if !slices.Equal(runner.argv, []string{securityBinary, "-i"}) {
		t.Errorf("argv = %v; want [%s -i]", runner.argv, securityBinary)
	}
	script := regexp.MustCompile(`^add-generic-password -U -s akiflow-test -a akiflow-test-account -X [0-9a-f]+\n$`)
	if !script.MatchString(runner.stdin) {
		t.Fatalf("stdin = %q; want add-generic-password script", runner.stdin)
	}
	_, after, _ := strings.Cut(runner.stdin, "-X ")
	decoded, err := hex.DecodeString(strings.TrimSuffix(after, "\n"))
	if err != nil {
		t.Fatalf("decode hex: %v", err)
	}
	if string(decoded) != token {
		t.Errorf("decoded credential = %q; want %q", decoded, token)
	}
	if strings.Contains(strings.Join(runner.argv, " "), token) {
		t.Errorf("argv leaks the credential: %v", runner.argv)
	}
}

func TestKeychainWriteFailureReturnsStderr(t *testing.T) {
	runner := &recordingRunner{result: CommandResult{exitCode: 1, stderr: []byte("denied by Keychain")}}
	store, err := NewKeychainStore("service", "account", runner.run)
	if err != nil {
		t.Fatalf("NewKeychainStore: %v", err)
	}
	err = store.Set(t.Context(), "token")
	if err == nil || !strings.Contains(err.Error(), "Keychain write failed: denied by Keychain") {
		t.Fatalf("set error = %v; want keychain write failure with stderr", err)
	}
}

func TestKeychainNamesRejectWhitespaceAndQuotes(t *testing.T) {
	for _, service := range []string{"bad service", "bad'service", `bad"service`} {
		if _, err := NewKeychainStore(service, "account", nil); err == nil || !strings.Contains(err.Error(), "Invalid Keychain service") {
			t.Errorf("service %q error = %v; want invalid keychain service", service, err)
		}
	}
	for _, account := range []string{"bad account", "bad'account", `bad"account`} {
		if _, err := NewKeychainStore("service", account, nil); err == nil || !strings.Contains(err.Error(), "Invalid Keychain account") {
			t.Errorf("account %q error = %v; want invalid keychain account", account, err)
		}
	}
}

func TestOAuthCallbackHandler(t *testing.T) {
	callback := func(target *url.URL) CallbackResult {
		if target.Query().Get("state") == "expected" {
			return CallbackResult{Status: CallbackSucceeded}
		}
		return CallbackResult{Status: CallbackIgnored}
	}
	t.Run("returns 400 for a wrong or missing state", func(t *testing.T) {
		if status, _ := handleCallbackRequest(mustParseURL(t, "http://127.0.0.1/callback?state=wrong"), callback); status != 400 {
			t.Errorf("wrong state status = %d; want 400", status)
		}
		if status, _ := handleCallbackRequest(mustParseURL(t, "http://127.0.0.1/callback"), callback); status != 400 {
			t.Errorf("missing state status = %d; want 400", status)
		}
	})
	t.Run("returns 404 for another path and 200 for the expected state", func(t *testing.T) {
		if status, _ := handleCallbackRequest(mustParseURL(t, "http://127.0.0.1/other?state=expected"), callback); status != 404 {
			t.Errorf("other path status = %d; want 404", status)
		}
		if status, _ := handleCallbackRequest(mustParseURL(t, "http://127.0.0.1/callback?state=expected"), callback); status != 200 {
			t.Errorf("expected state status = %d; want 200", status)
		}
	})
}

// mustParseURL parses raw or fails the test.
func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return parsed
}
