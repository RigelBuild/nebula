package client

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/RigelBuild/nebula/tools/akiflow/internal/core"
)

func TestCLIToolsListsNamesDescriptionsAndRequiredFields(t *testing.T) {
	rec := &recorder{}
	listed := []any{
		map[string]any{
			"name":        "add-task",
			"description": "Create a task\nAdditional details",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"title": map[string]any{}, "project": map[string]any{}},
				"required":   []any{"title", "project"},
			},
		},
		map[string]any{
			"name":        "search",
			"description": "Find tasks",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "required": []any{}},
		},
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := captureRequest(t, rec, r)
		switch c.method {
		case "initialize":
			writeRPC(t, w, c, map[string]any{"protocolVersion": "2025-06-18"}, nil)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			writeRPC(t, w, c, map[string]any{"tools": listed}, nil)
		}
	})
	app := newTestApp(t, handler, &stubStore{value: storedTokensJSON}, 0)
	stdout := &bytes.Buffer{}

	if err := Dispatch(t.Context(), []string{"tools"}, app, stdout); err != nil {
		t.Fatalf("tools: %v", err)
	}
	want := []string{"initialize", "notifications/initialized", "tools/list"}
	if got := methodsOf(rec.snapshot()); !slices.Equal(got, want) {
		t.Errorf("methods = %v; want %v", got, want)
	}
	wantLines := []string{
		"add-task\tCreate a task\trequired: title, project",
		"search\tFind tasks\trequired: (none)",
	}
	if got := outputLines(stdout); !slices.Equal(got, wantLines) {
		t.Errorf("stdout = %v; want %v", got, wantLines)
	}
}

func TestCLIRefreshesExpiredTokenBeforeFirstMCPRequest(t *testing.T) {
	rec := &recorder{}
	store := &stubStore{value: expiredTokensJSON}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := captureRequest(t, rec, r)
		if c.path == tokenPath {
			writeJSONResponse(t, w, map[string]any{"access_token": "fresh-access", "expires_in": 500}, nil)
			return
		}
		switch c.method {
		case "initialize":
			writeRPC(t, w, c, map[string]any{"protocolVersion": "2025-06-18"}, nil)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			writeRPC(t, w, c, map[string]any{"tools": taskToolList()}, nil)
		}
	})
	app := newTestApp(t, handler, store, 10000)

	if err := Dispatch(t.Context(), []string{"tools"}, app, &bytes.Buffer{}); err != nil {
		t.Fatalf("tools: %v", err)
	}
	calls := rec.snapshot()
	if calls[0].path != tokenPath || calls[1].path != mcpPath {
		t.Fatalf("request order = %v; want token refresh then mcp", pathsOf(calls))
	}
	if calls[1].auth != "Bearer fresh-access" {
		t.Errorf("initialize authorization = %q; want Bearer fresh-access", calls[1].auth)
	}
	var saved core.Tokens
	if err := json.Unmarshal([]byte(store.saved), &saved); err != nil {
		t.Fatalf("decode saved tokens: %v", err)
	}
	want := core.Tokens{ClientID: "client", AccessToken: "fresh-access", RefreshToken: "old-refresh", ExpiresAt: 510000}
	if saved != want {
		t.Errorf("saved tokens = %+v; want %+v", saved, want)
	}
}

func TestCLIDoesNotRetrySecond401AfterRefresh(t *testing.T) {
	rec := &recorder{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := captureRequest(t, rec, r)
		if c.path == tokenPath {
			writeJSONResponse(t, w, map[string]any{"access_token": "fresh-access", "expires_in": 500}, nil)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	})
	app := newTestApp(t, handler, &stubStore{value: storedTokensJSON}, 0)

	err := Dispatch(t.Context(), []string{"tools"}, app, &bytes.Buffer{})
	want := "Akiflow authorization failed after token refresh; run: akiflow login"
	if err == nil || err.Error() != want {
		t.Fatalf("tools error = %v; want %q", err, want)
	}
	calls := rec.snapshot()
	if got := countMethod(calls, "initialize"); got != 2 {
		t.Errorf("initialize count = %d; want 2", got)
	}
	if got := len(pathsOf(calls)) - countMethod(calls, "initialize"); got != 1 {
		t.Errorf("refresh count = %d; want 1", got)
	}
}

func TestCLIReportsLoginHintWithoutStoredToken(t *testing.T) {
	rec := &recorder{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captureRequest(t, rec, r)
		w.WriteHeader(http.StatusInternalServerError)
	})
	app := newTestApp(t, handler, &stubStore{}, 0)

	err := Dispatch(t.Context(), []string{"tools"}, app, &bytes.Buffer{})
	if err == nil || err.Error() != "No Akiflow login found; run: akiflow login" {
		t.Fatalf("tools error = %v; want the login hint", err)
	}
	if got := len(rec.snapshot()); got != 0 {
		t.Errorf("network requests = %d; want 0", got)
	}
}
