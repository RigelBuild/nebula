package client

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"testing"

	"github.com/RigelBuild/nebula/tools/akiflow/internal/core"
	"github.com/RigelBuild/nebula/tools/akiflow/internal/keychain"
)

func TestCLILoginOrchestration(t *testing.T) {
	rec := &recorder{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := captureRequest(t, rec, r)
		switch c.path {
		case registerPath:
			writeJSONResponse(t, w, map[string]any{"client_id": "dynamic-client"}, nil)
		case tokenPath:
			writeJSONResponse(t, w, map[string]any{
				"access_token":  "access-token",
				"refresh_token": "refresh-token",
				"expires_in":    120,
			}, nil)
		default:
			http.NotFound(w, r)
		}
	})
	store := &stubStore{}
	app := newTestApp(t, handler, store, 5000)
	var onCallback func(*url.URL) keychain.CallbackResult
	app.serve = func(_ int, callback func(*url.URL) keychain.CallbackResult) (func() error, error) {
		onCallback = callback
		return func() error { return nil }, nil
	}
	var authURL *url.URL
	var wrongState, rightState keychain.CallbackResult
	app.open = func(_ context.Context, target string) (int, error) {
		parsed, err := url.Parse(target)
		if err != nil {
			return 1, err
		}
		authURL = parsed
		state := parsed.Query().Get("state")
		wrongState = onCallback(mustParseURL(t, "http://127.0.0.1/callback?state=wrong&code=bad"))
		rightState = onCallback(mustParseURL(t, "http://127.0.0.1/callback?state="+url.QueryEscape(state)+"&code=authorization-code"))
		return 0, nil
	}
	stdout := &bytes.Buffer{}

	if err := Dispatch(t.Context(), []string{"login"}, app, stdout); err != nil {
		t.Fatalf("login: %v", err)
	}
	if wrongState.Status != keychain.CallbackIgnored {
		t.Errorf("wrong-state callback status = %v; want ignored", wrongState.Status)
	}
	if rightState.Status != keychain.CallbackSucceeded {
		t.Errorf("expected-state callback status = %v; want succeeded", rightState.Status)
	}
	calls := rec.snapshot()
	if got, want := pathsOf(calls), []string{registerPath, tokenPath}; !slices.Equal(got, want) {
		t.Errorf("requests = %v; want %v", got, want)
	}
	query := authURL.Query()
	if got := query.Get("client_id"); got != "dynamic-client" {
		t.Errorf("authorization client_id = %q; want dynamic-client", got)
	}
	if !challengePattern.MatchString(query.Get("code_challenge")) {
		t.Errorf("code_challenge = %q; want 43 base64url characters", query.Get("code_challenge"))
	}
	if got := query.Get("code_challenge_method"); got != "S256" {
		t.Errorf("code_challenge_method = %q; want S256", got)
	}
	if !challengePattern.MatchString(query.Get("state")) {
		t.Errorf("state = %q; want 43 base64url characters", query.Get("state"))
	}
	token := calls[1]
	if token.contentType != "application/x-www-form-urlencoded" {
		t.Errorf("token content-type = %q; want form", token.contentType)
	}
	if token.form.Get("grant_type") != "authorization_code" || token.form.Get("code") != "authorization-code" {
		t.Errorf("token form = %v; want authorization_code with code", token.form)
	}
	if token.form.Get("client_id") != "dynamic-client" || token.form.Get("code_verifier") == "" {
		t.Errorf("token form = %v; want client_id and code_verifier", token.form)
	}
	var saved core.Tokens
	if err := json.Unmarshal([]byte(store.saved), &saved); err != nil {
		t.Fatalf("decode saved tokens: %v", err)
	}
	want := core.Tokens{ClientID: "dynamic-client", AccessToken: "access-token", RefreshToken: "refresh-token", ExpiresAt: 125000}
	if saved != want {
		t.Errorf("saved tokens = %+v; want %+v", saved, want)
	}
}
