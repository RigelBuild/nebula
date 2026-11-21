package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const recallPath = "/v1/default/banks/example-bank/memories/recall"

func envOf(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func newServer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server.URL
}

func writeBody(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	if _, err := io.WriteString(w, body); err != nil {
		t.Errorf("write response: %v", err)
	}
}

func TestRunPrintsOneLinePerResult(t *testing.T) {
	base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != recallPath {
			t.Errorf("path = %s, want %s", r.URL.Path, recallPath)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q, want Bearer secret", got)
		}
		var body recallRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if body.Query != "dreamer lesson" || body.MaxTokens != 2000 {
			t.Errorf("request = %+v, want query and max_tokens 2000", body)
		}
		writeBody(t, w, `{"results":[{"id":"m1","text":"first\nline"},{"id":"m2","text":"second"}]}`)
	})
	var stdout bytes.Buffer
	args := []string{"--bank", "example-bank", "--query", "dreamer lesson", "--max-tokens", "2000"}
	env := envOf(map[string]string{"HINDSIGHT_URL": base, "HINDSIGHT_API_KEY": "secret"})
	if err := run(context.Background(), args, env, &stdout); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if got, want := stdout.String(), "m1\tfirst\\nline\nm2\tsecond\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestRunOmitsAuthorizationWithoutKey(t *testing.T) {
	base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got, ok := r.Header["Authorization"]; ok {
			t.Errorf("Authorization = %q, want no header", got)
		}
		writeBody(t, w, `{"results":[]}`)
	})
	var stdout bytes.Buffer
	args := []string{"--bank", "example-bank", "--query", "q", "--max-tokens", "10"}
	if err := run(context.Background(), args, envOf(map[string]string{"HINDSIGHT_URL": base}), &stdout); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want no lines for empty results", stdout.String())
	}
}

func TestRunEscapesBankIDInPath(t *testing.T) {
	base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.EscapedPath(), "/v1/default/banks/a%2Fb/memories/recall"; got != want {
			t.Errorf("escaped path = %s, want %s", got, want)
		}
		writeBody(t, w, `{"results":[]}`)
	})
	args := []string{"--bank", "a/b", "--query", "q", "--max-tokens", "10"}
	if err := run(context.Background(), args, envOf(map[string]string{"HINDSIGHT_URL": base}), io.Discard); err != nil {
		t.Fatalf("run() error = %v", err)
	}
}

func TestRunFailsNamingTheBank(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{name: "non-2xx", status: http.StatusInternalServerError, body: `{"detail":"boom"}`, wantErr: `bank "example-bank": recall returned HTTP 500`},
		{name: "malformed JSON", status: http.StatusOK, body: `{"results":`, wantErr: `bank "example-bank": malformed recall response`},
		{name: "missing results", status: http.StatusOK, body: `{}`, wantErr: `bank "example-bank": malformed recall response: missing results`},
		{name: "result without id", status: http.StatusOK, body: `{"results":[{"text":"x"}]}`, wantErr: "result 0 has no id"},
		{name: "result without text", status: http.StatusOK, body: `{"results":[{"id":"m1"}]}`, wantErr: "result 0 has no text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				writeBody(t, w, tt.body)
			})
			var stdout bytes.Buffer
			args := []string{"--bank", "example-bank", "--query", "q", "--max-tokens", "10"}
			err := run(context.Background(), args, envOf(map[string]string{"HINDSIGHT_URL": base}), &stdout)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("run() error = %v, want it to contain %q", err, tt.wantErr)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want nothing on failure", stdout.String())
			}
		})
	}
}

func TestRunRejectsBadFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "no arguments", args: nil},
		{name: "missing bank", args: []string{"--query", "q", "--max-tokens", "10"}},
		{name: "missing query", args: []string{"--bank", "b", "--max-tokens", "10"}},
		{name: "missing max-tokens", args: []string{"--bank", "b", "--query", "q"}},
		{name: "zero max-tokens", args: []string{"--bank", "b", "--query", "q", "--max-tokens", "0"}},
		{name: "unknown flag", args: []string{"--bogus"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := run(context.Background(), tt.args, envOf(nil), io.Discard)
			if _, ok := errors.AsType[usageError](err); !ok {
				t.Fatalf("run() error = %v, want usage error", err)
			}
		})
	}
}

func TestRunValidatesHindsightURL(t *testing.T) {
	tests := []struct {
		name    string
		base    string
		wantErr string
	}{
		{name: "unset", base: "", wantErr: "HINDSIGHT_URL is not set"},
		{name: "not http", base: "ftp://hindsight.example", wantErr: "must be an absolute http or https URL"},
		{name: "no host", base: "http://", wantErr: "must be an absolute http or https URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []string{"--bank", "b", "--query", "q", "--max-tokens", "10"}
			err := run(context.Background(), args, envOf(map[string]string{"HINDSIGHT_URL": tt.base}), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("run() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestRunRejectsUnsafeResultIDs(t *testing.T) {
	// A tab or newline in an id breaks the id<TAB>text contract, and a shell-hostile id reaches the
	// dreamer's shell. Each id must match ^[A-Za-z0-9_-]+$.
	tests := []struct {
		name string
		id   string
	}{
		{name: "tab", id: "m1\tx"},
		{name: "newline", id: "m1\nx"},
		{name: "dollar", id: "m$(id)"},
		{name: "backtick", id: "m`id`"},
		{name: "space", id: "m 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"results": []map[string]string{{"id": tt.id, "text": "x"}}})
			if err != nil {
				t.Fatalf("marshal body: %v", err)
			}
			base := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
				writeBody(t, w, string(body))
			})
			var stdout bytes.Buffer
			args := []string{"--bank", "b", "--query", "q", "--max-tokens", "10"}
			err = run(context.Background(), args, envOf(map[string]string{"HINDSIGHT_URL": base}), &stdout)
			if err == nil || !strings.Contains(err.Error(), "result 0 has an unsafe id") {
				t.Fatalf("run() error = %v, want unsafe id error", err)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want nothing when an id is unsafe", stdout.String())
			}
		})
	}
}

func TestRunAcceptsSafeResultIDs(t *testing.T) {
	base := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeBody(t, w, `{"results":[{"id":"8f14e45f-ceea-467f-a0e4-1b3d6f2a9c10","text":"x"}]}`)
	})
	var stdout bytes.Buffer
	args := []string{"--bank", "b", "--query", "q", "--max-tokens", "10"}
	if err := run(context.Background(), args, envOf(map[string]string{"HINDSIGHT_URL": base}), &stdout); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if got, want := stdout.String(), "8f14e45f-ceea-467f-a0e4-1b3d6f2a9c10\tx\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestRunRejectsHindsightURLWithQueryOrFragment(t *testing.T) {
	// The endpoint path is appended to the base URL, so a query or fragment would put it after them.
	for _, base := range []string{"https://hindsight.example/?x=1", "https://hindsight.example/#frag"} {
		t.Run(base, func(t *testing.T) {
			args := []string{"--bank", "b", "--query", "q", "--max-tokens", "10"}
			err := run(context.Background(), args, envOf(map[string]string{"HINDSIGHT_URL": base}), io.Discard)
			if err == nil || !strings.Contains(err.Error(), "must be an absolute http or https URL") {
				t.Fatalf("run() error = %v, want it to reject the query or fragment", err)
			}
		})
	}
}

func TestWriteResultsEscapesBackslash(t *testing.T) {
	// A literal backslash-n in memory text must not look like an escaped newline.
	var stdout bytes.Buffer
	results := []recallResult{{ID: new("m1"), Text: new(`a\nb`)}}
	if err := writeResults(&stdout, results); err != nil {
		t.Fatalf("writeResults() error = %v", err)
	}
	if got, want := stdout.String(), "m1\t"+`a\\nb`+"\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}
