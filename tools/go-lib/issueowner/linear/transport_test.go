package linear

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadAPIKey(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "key")
	if err := os.WriteFile(file, []byte("  file-key  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	getenv := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}
	tests := []struct {
		name   string
		values map[string]string
		want   string
	}{
		{name: "file beats env", values: map[string]string{"LINEAR_API_KEY_FILE": file, "LINEAR_API_KEY": "env-key"}, want: "file-key"},
		{name: "missing falls back", values: map[string]string{"LINEAR_API_KEY_FILE": filepath.Join(dir, "missing"), "LINEAR_API_KEY": "env-key"}, want: "env-key"},
		{name: "empty file falls back", values: map[string]string{"LINEAR_API_KEY_FILE": filepath.Join(dir, "empty"), "LINEAR_API_KEY": "env-key"}, want: "env-key"},
		{name: "directory falls back", values: map[string]string{"LINEAR_API_KEY_FILE": dir, "LINEAR_API_KEY": "env-key"}, want: "env-key"},
	}
	if err := os.WriteFile(filepath.Join(dir, "empty"), []byte(" \n\t "), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ReadAPIKey(getenv(test.values)); got != test.want {
				t.Fatalf("ReadAPIKey() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestNewIssueAPIWithTokenUsesBearerForFileToken(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "linear-token")
	if err := os.WriteFile(tokenFile, []byte("access-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer access-token" {
			t.Errorf("Authorization = %q, want Bearer access-token", got)
		}
		if _, err := io.WriteString(w, `{"data":{"issue":{"identifier":"RIG-42"}}}`); err != nil {
			t.Errorf("write issue response: %v", err)
		}
	}))
	defer server.Close()

	getenv := func(name string) string {
		if name == "LINEAR_API_KEY_FILE" {
			return tokenFile
		}
		return ""
	}
	token := ReadAPIKey(getenv)
	api := NewIssueAPIWithToken(server.Client(), server.URL, token)
	if _, err := api.FetchIssue(t.Context(), "RIG-42"); err != nil {
		t.Fatal(err)
	}
}

func TestNewIssueAPISelection(t *testing.T) {
	client := &http.Client{}
	tests := []struct {
		name      string
		env       map[string]string
		wantError string
		wantAuth  string
	}{
		{name: "no credential", wantError: "no Linear credential: set LINEAR_CLIENT_ID + LINEAR_CLIENT_SECRET (app-actor grant) or LINEAR_API_KEY"},
		{name: "api key", env: map[string]string{"LINEAR_API_KEY": "lin_api_fake"}, wantAuth: "lin_api_fake"},
		{name: "partial client pair", env: map[string]string{"LINEAR_CLIENT_ID": "real-id", "LINEAR_API_KEY": "fallback"}, wantError: "partial app-actor credential: LINEAR_CLIENT_ID and LINEAR_CLIENT_SECRET must be set together (one is set, the other is empty or missing). Set both, or unset both to use LINEAR_API_KEY"},
		{name: "empty client half", env: map[string]string{"LINEAR_CLIENT_ID": "", "LINEAR_CLIENT_SECRET": "real-secret", "LINEAR_API_KEY": "fallback"}, wantError: "partial app-actor credential"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			getenv := func(key string) string { return test.env[key] }
			api, err := NewIssueAPI(t.Context(), client, "https://example.invalid/graphql", getenv)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error = %v, want containing %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			concrete, ok := api.(*IssueClient)
			if !ok || concrete.authorization != test.wantAuth {
				t.Fatalf("API = %#v, authorization %q", api, concrete.authorization)
			}
		})
	}
}

func TestLinearGraphQLTransport(t *testing.T) {
	tests := []struct {
		name                                              string
		status                                            int
		body                                              string
		headers                                           map[string]string
		wantRateLimit                                     bool
		wantErr                                           string
		wantRetry, wantRequestsReset, wantComplexityReset *float64
	}{
		{name: "http 429 with header numbers", status: http.StatusTooManyRequests, body: `{"data":{}}`, headers: map[string]string{"Retry-After": "2.5", "X-RateLimit-Requests-Reset": "1700000000000", "X-RateLimit-Complexity-Reset": "1700000000100"}, wantRateLimit: true, wantRetry: new(2.5), wantRequestsReset: new(float64(1700000000000)), wantComplexityReset: new(float64(1700000000100))},
		{name: "http 429 with malformed numeric headers", status: http.StatusTooManyRequests, body: `{"data":{}}`, headers: map[string]string{"Retry-After": "NaN", "X-RateLimit-Requests-Reset": "not-a-number"}, wantRateLimit: true},
		{name: "non-JSON http 429 remains rate limited", status: http.StatusTooManyRequests, body: "upstream unavailable", wantRateLimit: true},
		{name: "HTTP 429 with ordinary first graphql error is not rate limited", status: http.StatusTooManyRequests, body: `{"errors":[{"message":"first","extensions":{"type":"Other"}}]}`, wantErr: "429"},
		{name: "graphql rate limit via extensions type", status: http.StatusBadRequest, body: `{"errors":[{"message":"quota","extensions":{"type":"ratelimited"}}]}`, headers: map[string]string{"X-RateLimit-Requests-Reset": "1700000000000"}, wantRateLimit: true, wantRequestsReset: new(float64(1700000000000))},
		{name: "graphql top-level type does not classify", status: http.StatusBadRequest, body: `{"errors":[{"message":"quota","type":"ratelimited"}]}`, wantErr: "quota"},
		{name: "graphql rate limit type is case sensitive", status: http.StatusBadRequest, body: `{"errors":[{"message":"quota","extensions":{"type":"Ratelimited"}}]}`, wantErr: "quota"},
		{name: "not rate limited by a later graphql error", status: http.StatusBadRequest, body: `{"errors":[{"message":"first","extensions":{"type":"Other"}},{"message":"quota","extensions":{"type":"ratelimited"}}]}`, wantErr: "first"},
		{name: "other HTTP error", status: http.StatusForbidden, body: `{"errors":[{"message":"denied"}]}`, wantErr: "403"},
		{name: "graphql error", status: http.StatusOK, body: `{"errors":[{"message":"bad query"}]}`, wantErr: "bad query"},
		{name: "malformed successful response reports decode error", status: http.StatusOK, body: "not-json", wantErr: "decoding Linear GraphQL response"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertLinearTransportResult(t, test.status, test.body, test.headers, test.wantRateLimit, test.wantErr, test.wantRetry, test.wantRequestsReset, test.wantComplexityReset)
		})
	}
}

func assertLinearTransportResult(t *testing.T, status int, body string, headers map[string]string, wantRateLimit bool, wantErr string, wantRetry, wantRequestsReset, wantComplexityReset *float64) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/graphql" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "test-key" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		for key, value := range headers {
			w.Header().Set(key, value)
		}
		w.WriteHeader(status)
		if _, err := io.WriteString(w, body); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer server.Close()
	api, err := NewIssueAPI(t.Context(), server.Client(), server.URL+"/graphql", func(name string) string {
		if name == "LINEAR_API_KEY" {
			return "test-key"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	concrete := api.(*IssueClient)
	var result map[string]any
	err = concrete.request(t.Context(), "query Test { issues { nodes { identifier } } }", map[string]any{}, &result)
	if wantRateLimit {
		var rate *RateLimitError
		if !errors.As(err, &rate) {
			t.Fatalf("error = %v, want RateLimitError", err)
		}
		assertOptionalFloat(t, rate.RetryAfter, wantRetry)
		assertOptionalFloat(t, rate.RequestsResetAt, wantRequestsReset)
		assertOptionalFloat(t, rate.ComplexityResetAt, wantComplexityReset)
		return
	}
	if wantErr != "" {
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Fatalf("error = %v, want %q", err, wantErr)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
}

func assertOptionalFloat(t *testing.T, got, want *float64) {
	t.Helper()
	if got == nil || want == nil {
		if got != nil || want != nil {
			t.Fatalf("optional number = %v, want %v", got, want)
		}
		return
	}
	if *got != *want {
		t.Fatalf("optional number = %v, want %v", *got, *want)
	}
}
func TestMintedCredentialUsesBearer(t *testing.T) {
	// Redirect the token request's host to the test server while retaining its
	// production URL in the implementation.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			if r.FormValue("scope") != "read,write" {
				t.Errorf("scope = %q", r.FormValue("scope"))
			}
			if err := r.ParseForm(); err != nil {
				t.Errorf("parse token form: %v", err)
			}
			_, err := io.WriteString(w, `{"access_token":"minted"}`)
			if err != nil {
				t.Errorf("write token: %v", err)
			}
			return
		}
		if r.Header.Get("Authorization") != "Bearer minted" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		_, err := io.WriteString(w, `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}`)
		if err != nil {
			t.Errorf("write page: %v", err)
		}
	}))
	defer server.Close()
	client := &http.Client{Transport: hostRewriteTransport{base: server.Client().Transport, host: strings.TrimPrefix(server.URL, "http://")}}
	api, err := NewIssueAPI(t.Context(), client, server.URL+"/graphql", func(name string) string {
		switch name {
		case "LINEAR_CLIENT_ID":
			return "client-id"
		case "LINEAR_CLIENT_SECRET":
			return "client-secret"
		case "LINEAR_API_KEY":
			return "ignored-key"
		default:
			return ""
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.FetchPage(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
}

type hostRewriteTransport struct {
	base http.RoundTripper
	host string
}

func (transport hostRewriteTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = transport.host
	clone.Host = transport.host
	return transport.base.RoundTrip(clone)
}
