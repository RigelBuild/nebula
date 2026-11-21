package kube

import (
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// seenRequest is one request the test apiserver received.
type seenRequest struct {
	method, path, contentType, auth, body string
}

// startAPIServer serves 200 to every request and reports each one on the returned channel.
func startAPIServer(t *testing.T) (*httptest.Server, chan seenRequest) {
	t.Helper()
	seen := make(chan seenRequest, 16)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, readErr := io.ReadAll(r.Body)
		seen <- seenRequest{
			method:      r.Method,
			path:        r.URL.Path,
			contentType: r.Header.Get("Content-Type"),
			auth:        r.Header.Get("Authorization"),
			body:        string(raw) + readErrSuffix(readErr),
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	return server, seen
}

// readErrSuffix reports a request body read failure inside the recorded body, so a test sees it.
func readErrSuffix(err error) string {
	if err == nil {
		return ""
	}
	return " [read error: " + err.Error() + "]"
}

func serverHostPort(t *testing.T, server *httptest.Server) (string, string) {
	t.Helper()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Hostname(), u.Port()
}

// serverEnv returns the getenv that points the client at server.
func serverEnv(t *testing.T, server *httptest.Server) func(string) string {
	t.Helper()
	host, port := serverHostPort(t, server)
	env := map[string]string{"KUBERNETES_SERVICE_HOST": host, "KUBERNETES_SERVICE_PORT": port}
	return func(key string) string { return env[key] }
}

func serverCA(server *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
}

// newServiceAccount returns a service-account dir whose ca.crt trusts server.
func newServiceAccount(t *testing.T, server *httptest.Server) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "ca.crt", serverCA(server))
	return dir
}

func writeFile(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// newTestClient builds a client for a fresh test apiserver, with token in its service-account dir.
func newTestClient(t *testing.T, token string) (Client, string, chan seenRequest) {
	t.Helper()
	server, seen := startAPIServer(t)
	dir := newServiceAccount(t, server)
	writeFile(t, dir, "token", []byte(token))
	client, err := newInClusterClient(serverEnv(t, server), os.DirFS(dir))
	if err != nil {
		t.Fatal(err)
	}
	return client, dir, seen
}

// TestRequestContentType checks the Content-Type and body each method sends.
func TestRequestContentType(t *testing.T) {
	t.Parallel()
	body := map[string]any{"unschedulable": true}
	cases := []struct {
		name, method, contentType, wantBody string
		send                                func(Client) (Response, error)
	}{
		{"GET has no content type or body", http.MethodGet, "", "", func(c Client) (Response, error) {
			return c.Get("/api/v1/nodes")
		}},
		{"PATCH is merge-patch", http.MethodPatch, "application/merge-patch+json", `{"unschedulable":true}`, func(c Client) (Response, error) {
			return c.Patch("/api/v1/nodes/a", body)
		}},
		{"POST is JSON", http.MethodPost, "application/json", `{"unschedulable":true}`, func(c Client) (Response, error) {
			return c.Post("/api/v1/namespaces/ns/pods/p/eviction", body)
		}},
		{"DELETE is merge-patch", http.MethodDelete, "application/merge-patch+json", `{"unschedulable":true}`, func(c Client) (Response, error) {
			return c.Delete("/api/v1/nodes/a", body)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client, _, seen := newTestClient(t, "token")
			if _, err := tc.send(client); err != nil {
				t.Fatal(err)
			}
			got := <-seen
			if got.method != tc.method || got.contentType != tc.contentType || got.body != tc.wantBody {
				t.Fatalf("request = %+v, want method %s, content type %q, body %q", got, tc.method, tc.contentType, tc.wantBody)
			}
		})
	}
}

// TestAuthorizationTrimsTheToken sends the token without the whitespace around it in the file.
func TestAuthorizationTrimsTheToken(t *testing.T) {
	t.Parallel()
	client, _, seen := newTestClient(t, " \t s3cret-token\r\n")
	if _, err := client.Get("/api/v1/nodes"); err != nil {
		t.Fatal(err)
	}
	if got := (<-seen).auth; got != "Bearer s3cret-token" {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer s3cret-token")
	}
}

// TestTokenIsReadOnEachRequest sends a rotated token without rebuilding the client.
func TestTokenIsReadOnEachRequest(t *testing.T) {
	t.Parallel()
	client, dir, seen := newTestClient(t, "first")
	if _, err := client.Get("/api/v1/nodes"); err != nil {
		t.Fatal(err)
	}
	if got := (<-seen).auth; got != "Bearer first" {
		t.Fatalf("first Authorization = %q, want %q", got, "Bearer first")
	}
	writeFile(t, dir, "token", []byte("second"))
	if _, err := client.Get("/api/v1/nodes"); err != nil {
		t.Fatal(err)
	}
	if got := (<-seen).auth; got != "Bearer second" {
		t.Fatalf("second Authorization = %q, want %q", got, "Bearer second")
	}
}

// TestNewInClusterClientRejectsBadStartup covers the startup failures that abort the run.
func TestNewInClusterClientRejectsBadStartup(t *testing.T) {
	t.Parallel()
	server, _ := startAPIServer(t)
	host, port := serverHostPort(t, server)
	full := map[string]string{"KUBERNETES_SERVICE_HOST": host, "KUBERNETES_SERVICE_PORT": port}
	cases := []struct {
		name    string
		env     map[string]string
		ca      []byte // nil writes no ca.crt
		wantErr string
	}{
		{"missing host", map[string]string{"KUBERNETES_SERVICE_PORT": port}, serverCA(server), "Kubernetes service environment is missing"},
		{"missing port", map[string]string{"KUBERNETES_SERVICE_HOST": host}, serverCA(server), "Kubernetes service environment is missing"},
		{"missing CA file", full, nil, "reading service account CA"},
		{"CA is not PEM", full, []byte("not a certificate"), "no certificates found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if tc.ca != nil {
				writeFile(t, dir, "ca.crt", tc.ca)
			}
			getenv := func(key string) string { return tc.env[key] }
			if _, err := newInClusterClient(getenv, os.DirFS(dir)); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestMissingTokenFailsTheRequestNotTheConstructor reads the token per request, so a missing file fails the first call.
func TestMissingTokenFailsTheRequestNotTheConstructor(t *testing.T) {
	t.Parallel()
	server, seen := startAPIServer(t)
	dir := newServiceAccount(t, server)
	client, err := newInClusterClient(serverEnv(t, server), os.DirFS(dir))
	if err != nil {
		t.Fatalf("constructor error = %v, want none", err)
	}
	if _, err := client.Get("/api/v1/nodes"); err == nil || !strings.Contains(err.Error(), "reading service account token") {
		t.Fatalf("Get error = %v, want one containing %q", err, "reading service account token")
	}
	select {
	case got := <-seen:
		t.Fatalf("request reached the apiserver: %+v", got)
	default:
	}
}

// TestRequestDoesNotFollowRedirects: a 3xx from the apiserver must not send the
// service-account token to the redirect target. The client returns the 3xx as its
// response, so the caller's non-2xx path reports it.
func TestRequestDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		status int
	}{
		{name: "301 moved permanently", status: http.StatusMovedPermanently},
		{name: "302 found", status: http.StatusFound},
		{name: "307 temporary redirect", status: http.StatusTemporaryRedirect},
		{name: "308 permanent redirect", status: http.StatusPermanentRedirect},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var targetRequests, targetAuthorized atomic.Int32
			target := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				targetRequests.Add(1)
				if r.Header.Get("Authorization") != "" {
					targetAuthorized.Add(1)
				}
			}))
			t.Cleanup(target.Close)
			redirector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL+r.URL.Path, tt.status)
			}))
			t.Cleanup(redirector.Close)

			const token = "service-account-token"
			dir := t.TempDir()
			// The CA bundle trusts both servers, so a followed redirect reaches the
			// target instead of failing the TLS handshake.
			writeFile(t, dir, "ca.crt", append(serverCA(redirector), serverCA(target)...))
			writeFile(t, dir, "token", []byte(token))
			client, err := newInClusterClient(serverEnv(t, redirector), os.DirFS(dir))
			if err != nil {
				t.Fatal(err)
			}

			resp, err := client.Post("/api/v1/namespaces/ns/pods/p/eviction", map[string]any{"unschedulable": true})
			if n := targetRequests.Load(); n != 0 {
				t.Errorf("redirect target received %d requests, %d with Authorization; want 0", n, targetAuthorized.Load())
			}
			if err != nil {
				t.Fatalf("Post() error = %v, want the %d returned as a response", err, tt.status)
			}
			if resp.Status != tt.status || resp.OK() {
				t.Fatalf("status = %d, want %d as a non-2xx response", resp.Status, tt.status)
			}
			if strings.Contains(string(resp.Body), token) {
				t.Errorf("response body %q contains the bearer token", resp.Body)
			}
		})
	}
}
