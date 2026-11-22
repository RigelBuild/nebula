package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"testing"

	"github.com/RigelBuild/nebula/tools/akiflow/internal/core"
)

func TestCLIAddInitializesAndAddsTask(t *testing.T) {
	rec := &recorder{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := captureRequest(t, rec, r)
		switch c.method {
		case "initialize":
			writeRPC(t, w, c, map[string]any{"protocolVersion": "2025-03-26"}, map[string]string{"mcp-session-id": "session-1"})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			writeRPC(t, w, c, map[string]any{"tools": taskToolList()}, nil)
		case "tools/call":
			writeRPC(t, w, c, map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}, nil)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	app := newTestApp(t, handler, &stubStore{value: storedTokensJSON}, 0)
	stdout := &bytes.Buffer{}

	if err := Dispatch(t.Context(), []string{"add", "buy", "milk"}, app, stdout); err != nil {
		t.Fatalf("add: %v", err)
	}
	if got := outputLines(stdout); !slices.Equal(got, []string{"Added: buy milk"}) {
		t.Errorf("stdout = %v; want Added: buy milk", got)
	}
	calls := rec.snapshot()
	want := []string{"initialize", "notifications/initialized", "tools/list", "tools/call"}
	if got := methodsOf(calls); !slices.Equal(got, want) {
		t.Errorf("methods = %v; want %v", got, want)
	}
	for _, c := range calls[1:] {
		if c.protocol != "2025-03-26" {
			t.Errorf("%s protocol header = %q; want 2025-03-26", c.method, c.protocol)
		}
		if c.session != "session-1" {
			t.Errorf("%s session header = %q; want session-1", c.method, c.session)
		}
	}
	wantParams := map[string]any{"name": "add-task", "arguments": map[string]any{"title": "buy milk"}}
	if !reflect.DeepEqual(calls[3].params, wantParams) {
		t.Errorf("tools/call params = %v; want %v", calls[3].params, wantParams)
	}
}

func TestCLIAddReinitializesOnceWhenSessionExpires(t *testing.T) {
	rec := &recorder{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := captureRequest(t, rec, r)
		switch c.method {
		case "initialize":
			n := countMethod(rec.snapshot(), "initialize")
			writeRPC(t, w, c, map[string]any{"protocolVersion": "2025-06-18"}, map[string]string{"mcp-session-id": fmt.Sprintf("session-%d", n)})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			writeRPC(t, w, c, map[string]any{"tools": taskToolList()}, nil)
		case "tools/call":
			if c.session == "session-1" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			writeRPC(t, w, c, map[string]any{"content": []any{}}, nil)
		}
	})
	app := newTestApp(t, handler, &stubStore{value: storedTokensJSON}, 0)
	stdout := &bytes.Buffer{}

	if err := Dispatch(t.Context(), []string{"add", "buy", "milk"}, app, stdout); err != nil {
		t.Fatalf("add: %v", err)
	}
	if got := outputLines(stdout); !slices.Equal(got, []string{"Added: buy milk"}) {
		t.Errorf("stdout = %v; want Added: buy milk", got)
	}
	var sessions []string
	for _, c := range rec.snapshot() {
		if c.method == "tools/call" {
			sessions = append(sessions, c.session)
		}
	}
	if want := []string{"session-1", "session-2"}; !slices.Equal(sessions, want) {
		t.Errorf("tools/call sessions = %v; want %v", sessions, want)
	}
}

func TestCLIAddFailsInsteadOfLoopingWhenFreshSessionAlsoExpires(t *testing.T) {
	rec := &recorder{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := captureRequest(t, rec, r)
		switch c.method {
		case "initialize":
			n := countMethod(rec.snapshot(), "initialize")
			writeRPC(t, w, c, map[string]any{"protocolVersion": "2025-06-18"}, map[string]string{"mcp-session-id": fmt.Sprintf("session-%d", n)})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	app := newTestApp(t, handler, &stubStore{value: storedTokensJSON}, 0)

	err := Dispatch(t.Context(), []string{"tools"}, app, &bytes.Buffer{})
	if err == nil || err.Error() != "Akiflow MCP returned HTTP 404" {
		t.Fatalf("tools error = %v; want Akiflow MCP returned HTTP 404", err)
	}
	if got := countMethod(rec.snapshot(), "initialize"); got != 2 {
		t.Errorf("initialize count = %d; want 2", got)
	}
}

func TestCLIDoesNotRetryInitializeNotFound(t *testing.T) {
	rec := &recorder{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captureRequest(t, rec, r)
		w.WriteHeader(http.StatusNotFound)
	})
	app := newTestApp(t, handler, &stubStore{value: storedTokensJSON}, 0)

	if err := Dispatch(t.Context(), []string{"tools"}, app, &bytes.Buffer{}); err == nil {
		t.Fatal("tools error = nil; want a 404 failure")
	}
	if got := countMethod(rec.snapshot(), "initialize"); got != 1 {
		t.Errorf("initialize count = %d; want 1", got)
	}
}

func TestCLIAddReturnsToolErrorOnFailure(t *testing.T) {
	rec := &recorder{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := captureRequest(t, rec, r)
		switch c.method {
		case "initialize":
			writeRPC(t, w, c, map[string]any{"protocolVersion": "2025-06-18"}, nil)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			writeRPC(t, w, c, map[string]any{"tools": taskToolList()}, nil)
		case "tools/call":
			writeRPC(t, w, c, map[string]any{
				"isError": true,
				"content": []any{map[string]any{"type": "text", "text": "Akiflow rejected this title"}},
			}, nil)
		}
	})
	app := newTestApp(t, handler, &stubStore{value: storedTokensJSON}, 0)
	stdout := &bytes.Buffer{}

	err := Dispatch(t.Context(), []string{"add", "bad"}, app, stdout)
	if err == nil || err.Error() != "Akiflow rejected this title" {
		t.Fatalf("add error = %v; want Akiflow rejected this title", err)
	}
	if got := outputLines(stdout); got != nil {
		t.Errorf("stdout = %v; want none", got)
	}
}

func TestCLIAddRefreshesAfterUnauthorizedAndRetries(t *testing.T) {
	rec := &recorder{}
	store := &stubStore{value: storedTokensJSON}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := captureRequest(t, rec, r)
		if c.path == tokenPath {
			writeJSONResponse(t, w, map[string]any{"access_token": "new-access", "expires_in": 500}, nil)
			return
		}
		switch c.method {
		case "initialize":
			writeRPC(t, w, c, map[string]any{"protocolVersion": "2025-06-18"}, nil)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			writeRPC(t, w, c, map[string]any{"tools": taskToolList()}, nil)
		case "tools/call":
			if countMethod(rec.snapshot(), "tools/call") == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			writeRPC(t, w, c, map[string]any{"content": []any{}}, nil)
		}
	})
	app := newTestApp(t, handler, store, 0)
	stdout := &bytes.Buffer{}

	if err := Dispatch(t.Context(), []string{"add", "refresh test"}, app, stdout); err != nil {
		t.Fatalf("add: %v", err)
	}
	var used []string
	var callProtocols []string
	for _, c := range rec.snapshot() {
		if c.method == "tools/call" {
			used = append(used, c.auth)
			callProtocols = append(callProtocols, c.protocol)
		}
	}
	if want := []string{"Bearer old-access", "Bearer new-access"}; !slices.Equal(used, want) {
		t.Errorf("tools/call authorization = %v; want %v", used, want)
	}
	for _, protocol := range callProtocols {
		if protocol != "2025-06-18" {
			t.Errorf("tools/call protocol header = %q; want 2025-06-18", protocol)
		}
	}
	var refreshCall callRecord
	for _, c := range rec.snapshot() {
		if c.path == tokenPath {
			refreshCall = c
		}
	}
	if refreshCall.form.Get("grant_type") != "refresh_token" || refreshCall.form.Get("refresh_token") != "old-refresh" {
		t.Errorf("refresh form = %v; want refresh_token grant with old-refresh", refreshCall.form)
	}
	var saved core.Tokens
	if err := json.Unmarshal([]byte(store.saved), &saved); err != nil {
		t.Fatalf("decode saved tokens: %v", err)
	}
	want := core.Tokens{ClientID: "client", AccessToken: "new-access", RefreshToken: "old-refresh", ExpiresAt: 500000}
	if saved != want {
		t.Errorf("saved tokens = %+v; want %+v", saved, want)
	}
	if got := outputLines(stdout); !slices.Equal(got, []string{"Added: refresh test"}) {
		t.Errorf("stdout = %v; want Added: refresh test", got)
	}
}

// TestCLIAddPassesDashHAsTitle: argv is not parsed as a flag, so -h is a task title.
func TestCLIAddPassesDashHAsTitle(t *testing.T) {
	rec := &recorder{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := captureRequest(t, rec, r)
		switch c.method {
		case "initialize":
			writeRPC(t, w, c, map[string]any{"protocolVersion": "2025-03-26"}, map[string]string{"mcp-session-id": "session-1"})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			writeRPC(t, w, c, map[string]any{"tools": taskToolList()}, nil)
		case "tools/call":
			writeRPC(t, w, c, map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}, nil)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	app := newTestApp(t, handler, &stubStore{value: storedTokensJSON}, 0)
	stdout := &bytes.Buffer{}

	if err := Dispatch(t.Context(), []string{"add", "-h"}, app, stdout); err != nil {
		t.Fatalf("add -h: %v", err)
	}
	if got := outputLines(stdout); !slices.Equal(got, []string{"Added: -h"}) {
		t.Errorf("stdout = %v; want Added: -h", got)
	}
	calls := rec.snapshot()
	wantParams := map[string]any{"name": "add-task", "arguments": map[string]any{"title": "-h"}}
	if !reflect.DeepEqual(calls[len(calls)-1].params, wantParams) {
		t.Errorf("tools/call params = %v; want %v", calls[len(calls)-1].params, wantParams)
	}
}
