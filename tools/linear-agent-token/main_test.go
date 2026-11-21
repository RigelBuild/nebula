package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type apiTransport struct {
	base http.RoundTripper
	host string
}

func (transport apiTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	copy := request.Clone(request.Context())
	copy.URL = &url.URL{Scheme: "http", Host: transport.host, Path: request.URL.Path}
	copy.Host = transport.host
	return transport.base.RoundTrip(copy)
}

func tokenFixture(t *testing.T, tokenStatus int, viewer, token string) (map[string]string, *apiTransport, *[]string, *[]string) {
	t.Helper()
	root := t.TempDir()
	clientIDPath := filepath.Join(root, "id")
	clientSecretPath := filepath.Join(root, "secret")
	if err := os.WriteFile(clientIDPath, []byte(" client-id\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(clientSecretPath, []byte("client-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var scopes []string
	var authorization []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			if err := r.ParseForm(); err != nil {
				t.Errorf("parse token form: %v", err)
			}
			scopes = append(scopes, r.Form.Get("scope"))
			w.Header().Set("Content-Type", "application/json")
			if tokenStatus != http.StatusOK {
				w.WriteHeader(tokenStatus)
				if _, err := w.Write([]byte(`{"error":"invalid_client"}`)); err != nil {
					t.Errorf("write token error: %v", err)
				}
				return
			}
			if _, err := w.Write([]byte(fmt.Sprintf(`{"access_token":%q}`, token))); err != nil {
				t.Errorf("write token: %v", err)
			}
		case "/graphql":
			authorization = append(authorization, r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "application/json")
			response := map[string]any{"data": map[string]any{"viewer": map[string]string{}}}
			if viewer != "" {
				response["data"].(map[string]any)["viewer"] = map[string]string{"id": viewer}
			}
			encoded, err := json.Marshal(response)
			if err != nil {
				t.Errorf("marshal viewer response: %v", err)
				return
			}
			if _, err := w.Write(encoded); err != nil {
				t.Errorf("write viewer response: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := &apiTransport{base: server.Client().Transport, host: serverURL.Host}
	state := filepath.Join(root, "state")
	values := map[string]string{
		"LINEAR_CLIENT_ID_FILE":     clientIDPath,
		"LINEAR_CLIENT_SECRET_FILE": clientSecretPath,
		"LINEAR_AGENT_TOKEN_FILE":   filepath.Join(state, "token"),
		"LINEAR_AGENT_USER_ID_FILE": filepath.Join(state, "user-id"),
	}
	return values, transport, &scopes, &authorization
}

func environmentFrom(values map[string]string) environment {
	return func(name string) string { return values[name] }
}

func TestMintToFiles(t *testing.T) {
	tests := []struct {
		name        string
		tokenStatus int
		token       string
		viewer      string
		wantErr     string
		keepOld     bool
	}{
		{name: "writes the token and app user id owner-only and always requests the fixed scope", tokenStatus: http.StatusOK, token: "tok-1", viewer: "app-user"},
		{name: "a failed mint keeps the previous token", tokenStatus: http.StatusUnauthorized, token: "tok-2", viewer: "app-user", wantErr: "401", keepOld: true},
		{name: "a token whose viewer has no id is not installed", tokenStatus: http.StatusOK, token: "tok-2", wantErr: "no id", keepOld: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values, transport, scopes, authorization := tokenFixture(t, test.tokenStatus, test.viewer, test.token)
			tokenFile, userIDFile := values["LINEAR_AGENT_TOKEN_FILE"], values["LINEAR_AGENT_USER_ID_FILE"]
			wantFiles := map[string]string{tokenFile: test.token + "\n", userIDFile: test.viewer + "\n"}
			if test.keepOld {
				wantFiles = map[string]string{tokenFile: "tok-1\n", userIDFile: "app-user\n"}
				seedFiles(t, wantFiles)
			}

			err := mintToFiles(t.Context(), environmentFrom(values), &http.Client{Transport: transport})
			assertErrContains(t, err, test.wantErr)
			if test.tokenStatus == http.StatusOK {
				if len(*scopes) != 1 || (*scopes)[0] != agentTokenScope {
					t.Errorf("requested scopes = %v, want [%q]", *scopes, agentTokenScope)
				}
				if test.viewer != "" && (len(*authorization) != 1 || (*authorization)[0] != test.token) {
					t.Errorf("viewer authorization = %v, want [%q]", *authorization, test.token)
				}
			}
			assertOwnerOnlyFiles(t, wantFiles)
			entries, readErr := os.ReadDir(filepath.Dir(tokenFile))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 2 {
				t.Errorf("state directory entries = %v, want only final outputs", entries)
			}
		})
	}
}

func seedFiles(t *testing.T, files map[string]string) {
	t.Helper()
	for path, value := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func assertOwnerOnlyFiles(t *testing.T, want map[string]string) {
	t.Helper()
	for path, contents := range want {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != contents {
			t.Errorf("%s = %q, want %q", filepath.Base(path), got, contents)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("%s mode = %#o, want 0600", filepath.Base(path), mode)
		}
	}
}

func assertErrContains(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want containing %q", err, want)
	}
}

func TestMintToFilesMissingCredentialsDoesNotRequestToken(t *testing.T) {
	t.Run("missing credentials fail before any request", func(t *testing.T) {
		values, transport, scopes, _ := tokenFixture(t, http.StatusOK, "app-user", "tok-1")
		values["LINEAR_CLIENT_ID_FILE"] = filepath.Join(t.TempDir(), "absent")
		err := mintToFiles(t.Context(), environmentFrom(values), &http.Client{Transport: transport})
		if err == nil || !strings.Contains(err.Error(), "client credentials are missing") {
			t.Fatalf("mintToFiles() error = %v, want missing credentials", err)
		}
		if len(*scopes) != 0 {
			t.Errorf("token requests = %v, want none", *scopes)
		}
	})
}

func TestRootCommandHelpAndArguments(t *testing.T) {
	for _, test := range []struct {
		name     string
		args     []string
		wantCode int
		wantText string
	}{
		{name: "help", args: []string{"--help"}, wantText: "Usage:"},
		{name: "rejects arguments", args: []string{"extra"}, wantCode: 1, wantText: "extra"},
		{name: "rejects completion subcommand", args: []string{"completion", "bash"}, wantCode: 1, wantText: "completion"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr strings.Builder
			command := newRootCommand(t.Context(), func(string) string { return "" }, &stdout, &stderr)
			command.SetArgs(test.args)
			code := 0
			if err := command.Execute(); err != nil {
				code = 1
				if !strings.Contains(err.Error(), test.wantText) {
					t.Fatalf("command error = %v, want containing %q", err, test.wantText)
				}
			}
			if code != test.wantCode {
				t.Errorf("exit code = %d, want %d", code, test.wantCode)
			}
			if test.wantCode == 0 && !strings.Contains(stdout.String(), test.wantText) {
				t.Errorf("help output = %q, want containing %q", stdout.String(), test.wantText)
			}
		})
	}
}
