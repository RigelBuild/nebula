package kube

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/RigelBuild/nebula/tools/garage-bucket-reconcile/internal/mint"
)

// roundTripFunc adapts a function to http.RoundTripper, so a test controls the request and reply.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// newTestClient writes a service account token and CA into a temp directory and returns a client that uses them.
func newTestClient(t *testing.T, rt http.RoundTripper) *Client {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("service-account-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), []byte("test-ca"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Client{
		baseURL:        "https://10.43.0.1:443",
		serviceAccount: dir,
		transport:      rt,
	}
}

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestReplaceSendsWholeVersionedSecretWithPutAndBearerAuth(t *testing.T) {
	t.Parallel()
	body := map[string]any{"metadata": map[string]any{"resourceVersion": "17"}, "data": map[string]any{"token": "encoded"}}
	var seenURL, seenMethod, seenAuth, seenType string
	var seenBody string
	c := newTestClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seenURL = r.URL.String()
		seenMethod = r.Method
		seenAuth = r.Header.Get("Authorization")
		seenType = r.Header.Get("Content-Type")
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		seenBody = string(raw)
		return response(http.StatusOK, ""), nil
	}))
	if err := c.Replace(context.Background(), "example-namespace", "scoped-token", body); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if seenURL != "https://10.43.0.1:443/api/v1/namespaces/example-namespace/secrets/scoped-token" {
		t.Errorf("url = %s", seenURL)
	}
	if seenMethod != http.MethodPut {
		t.Errorf("method = %s, want PUT", seenMethod)
	}
	if seenAuth != "Bearer service-account-token" {
		t.Errorf("authorization = %q", seenAuth)
	}
	if seenType != "application/json" {
		t.Errorf("content-type = %q", seenType)
	}
	if seenBody != `{"data":{"token":"encoded"},"metadata":{"resourceVersion":"17"}}` {
		t.Errorf("body = %s", seenBody)
	}
}

func TestReplaceMapsConflictToSecretConflict(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusConflict, "conflict"), nil
	}))
	err := c.Replace(context.Background(), "example-namespace", "scoped-token", map[string]any{})
	if !errors.Is(err, mint.ErrSecretConflict) {
		t.Fatalf("err = %v, want ErrSecretConflict", err)
	}
}

func TestReplaceRejectsOtherErrorsWithoutExposingResponseBody(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusInternalServerError, "sensitive-response-body"), nil
	}))
	err := c.Replace(context.Background(), "example-namespace", "scoped-token", map[string]any{})
	if err == nil {
		t.Fatal("Replace succeeded, want error")
	}
	if err.Error() != "Kubernetes Secret PUT failed with HTTP 500" {
		t.Errorf("err = %q, want fixed message", err.Error())
	}
	if strings.Contains(err.Error(), "sensitive-response-body") {
		t.Error("response body leaked into the error")
	}
}

// TestReplaceDoesNotFollowRedirects: the service account token rides on every request, so a
// 3xx must not make the client resend it. The redirect target must receive no request, and
// Replace must fail with the status, not succeed. It uses a real transport, because the
// redirect policy lives in the http.Client that wraps it.
func TestReplaceDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()
	var targetRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetRequests.Add(1)
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	c := newTestClient(t, http.DefaultTransport)
	c.baseURL = redirector.URL
	err := c.Replace(context.Background(), "example-namespace", "scoped-token", map[string]any{})
	if err == nil {
		t.Fatal("Replace succeeded through a redirect, want error")
	}
	if n := targetRequests.Load(); n != 0 {
		t.Errorf("redirect target received %d requests, want 0", n)
	}
	if strings.Contains(err.Error(), "service-account-token") {
		t.Error("error exposed the service account token")
	}
}

func TestNewBracketsIPv6ServiceHost(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		"KUBERNETES_SERVICE_HOST": "fd00::1",
		"KUBERNETES_SERVICE_PORT": "443",
	}
	c, err := New(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if want := "https://[fd00::1]:443"; c.baseURL != want {
		t.Errorf("baseURL = %q, want %q", c.baseURL, want)
	}
}
