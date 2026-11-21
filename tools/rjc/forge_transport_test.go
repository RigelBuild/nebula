package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestForgeFailure(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        []byte
		wantMessage string
		wantError   string
	}{
		{
			name:        "JSON message",
			status:      404,
			body:        []byte(`{"message":"Not Found"}`),
			wantMessage: "Not Found",
		},
		{
			name:        "plain text message",
			status:      503,
			body:        []byte("Service Unavailable"),
			wantMessage: "Service Unavailable",
			wantError:   "HTTP 503",
		},
		{
			name:        "empty body",
			status:      502,
			wantMessage: "Bad Gateway",
		},
		{
			name:        "whitespace-only body",
			status:      502,
			body:        []byte(" \n\t "),
			wantMessage: "Bad Gateway",
		},
		{
			name:        "unknown status empty body",
			status:      599,
			wantMessage: "no response body",
		},
		{
			name:        "empty JSON message",
			status:      503,
			body:        []byte(`{"message":""}`),
			wantMessage: `{"message":""}`,
		},
		{
			name:        "long body is capped",
			status:      503,
			body:        []byte(strings.Repeat("x", 250)),
			wantMessage: strings.Repeat("x", 197) + "…",
		},
		{
			name:        "multibyte body truncates at rune boundary",
			status:      503,
			body:        []byte(strings.Repeat("a", 196) + "🌍" + "trailing content"),
			wantMessage: strings.Repeat("a", 196) + "…",
		},
		{
			name:        "invalid UTF-8 body is replaced before truncation",
			status:      http.StatusServiceUnavailable,
			body:        []byte(strings.Repeat("\x80", 250)),
			wantMessage: "\uFFFD",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := forgeFailure(tt.status, tt.body, nil)
			apiErr, ok := errors.AsType[*forgeAPIError](err)
			if !ok {
				t.Fatalf("forgeFailure() error = %T, want *forgeAPIError", err)
			}
			if apiErr.status != tt.status {
				t.Errorf("status = %d, want %d", apiErr.status, tt.status)
			}
			if apiErr.message != tt.wantMessage {
				t.Errorf("message = %q, want %q", apiErr.message, tt.wantMessage)
			}
			if !utf8.ValidString(apiErr.message) {
				t.Errorf("message %q is not valid UTF-8", apiErr.message)
			}
			if len(apiErr.message) > forgeMessageLimit {
				t.Errorf("message length = %d bytes, want at most %d", len(apiErr.message), forgeMessageLimit)
			}
			if tt.wantError != "" && !strings.Contains(apiErr.Error(), tt.wantError) {
				t.Errorf("Error() = %q, want it to contain %q", apiErr.Error(), tt.wantError)
			}
		})
	}
}

func TestForgeClientReportsNonJSONFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		if _, err := w.Write([]byte("Service Unavailable")); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)

	client := &forgeClient{client: server.Client(), baseURL: server.URL}
	err := client.api(t.Context(), http.MethodGet, "/failure", nil, nil)
	apiErr, ok := errors.AsType[*forgeAPIError](err)
	if !ok {
		t.Fatalf("api() error = %T, want *forgeAPIError", err)
	}
	if apiErr.status != http.StatusServiceUnavailable || apiErr.message != "Service Unavailable" {
		t.Errorf("api() error = %#v, want HTTP 503 and Service Unavailable", apiErr)
	}
}

func ghAPIHelper() int {
	if os.Getenv("GH_TOKEN") != "fixture-token" || os.Getenv("GITHUB_TOKEN") != "" {
		return 93
	}
	path := os.Args[len(os.Args)-1]
	calls, err := os.OpenFile(os.Getenv("RJC_TEST_GH_CALLS"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 96
	}
	_, writeErr := fmt.Fprintln(calls, path)
	closeErr := calls.Close()
	if writeErr != nil || closeErr != nil {
		return 96
	}
	if path == "repos/example/app" {
		if _, err := fmt.Fprint(os.Stdout, "HTTP/2.0 200 OK\r\nContent-Type: application/json\r\n\r\n{\"full_name\":\"example/app\"}"); err != nil {
			return 94
		}
		return 0
	}
	sha, _, ok := strings.Cut(strings.TrimPrefix(path, "repos/example/app/commits/"), "/pulls")
	if !ok || !strings.Contains(strings.Join(os.Args, " "), "--include") {
		return 95
	}
	body, err := json.Marshal(map[string]string{"message": "No commit found for SHA: " + sha, "documentation_url": "https://docs.github.com/rest/commits/commits#list-pull-requests-associated-with-a-commit", "status": "422"})
	if err != nil {
		return 96
	}
	if _, err := fmt.Fprintf(os.Stdout, "HTTP/2.0 422 Unprocessable Entity\r\nContent-Type: application/json\r\n\r\n%s", body); err != nil {
		return 94
	}
	return 1
}

func TestAdoptionHandlesGHUnknownCommitResponse(t *testing.T) {
	f := fixtureRepo(t)
	f.commit(t, "own.txt")
	s, err := service(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.forge = &forgeClient{repo: f.repo, token: "fixture-token"}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.Symlink(executable, filepath.Join(bin, "gh")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RJC_TEST_HELPER", "1")
	calls := filepath.Join(bin, "calls")
	t.Setenv("RJC_TEST_GH_CALLS", calls)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := f.repo.newChange(f.ctx, newOptions{issue: "EX-2", title: "feat(rjc): adopt an unknown SHA through gh", at: "@-"}); err != nil {
		t.Fatal(err)
	}
	observed, err := os.ReadFile(calls)
	if err != nil || strings.Count(string(observed), "\n") != 2 {
		t.Fatalf("gh lookup calls=%q err=%v", observed, err)
	}
}
