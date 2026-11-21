// Package kube reads and replaces Kubernetes Secrets from inside the cluster.
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/RigelBuild/nebula/tools/garage-bucket-reconcile/internal/jscompat"
	"github.com/RigelBuild/nebula/tools/garage-bucket-reconcile/internal/mint"
)

// DefaultServiceAccount is the in-cluster service account mount.
const DefaultServiceAccount = "/var/run/secrets/kubernetes.io/serviceaccount"

// Client implements mint.SecretClient against the in-cluster API server.
type Client struct {
	baseURL        string
	serviceAccount string
	// transport is nil in production, where roundTripper builds a transport that trusts the service account CA.
	transport http.RoundTripper
}

// New builds a Client from the in-cluster service host and port.
func New(getenv func(string) string) (*Client, error) {
	host, ok := jscompat.Lookup(getenv, "KUBERNETES_SERVICE_HOST")
	if !ok {
		return nil, errors.New("KUBERNETES_SERVICE_HOST is missing from the in-cluster environment")
	}
	port, ok := jscompat.Lookup(getenv, "KUBERNETES_SERVICE_PORT")
	if !ok {
		return nil, errors.New("KUBERNETES_SERVICE_PORT is missing from the in-cluster environment")
	}
	return &Client{baseURL: "https://" + host + ":" + port, serviceAccount: DefaultServiceAccount}, nil
}

// secretPath builds the Secret resource path with encodeURIComponent-compatible escaping.
func secretPath(namespace, name string) string {
	return "/api/v1/namespaces/" + jscompat.EncodeURIComponent(namespace) + "/secrets/" + jscompat.EncodeURIComponent(name)
}

// Read fetches the Secret as a JSON object.
func (c *Client) Read(ctx context.Context, namespace, name string) (map[string]any, error) {
	text, err := c.do(ctx, http.MethodGet, namespace, name, nil)
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if err := json.Unmarshal(text, &value); err != nil {
		return nil, errors.New("Kubernetes Secret GET returned invalid JSON") //nolint:staticcheck // ST1005: user-facing message, text is fixed.
	}
	return value, nil
}

// Replace sends the whole versioned Secret with PUT. It returns mint.ErrSecretConflict on HTTP 409.
func (c *Client) Replace(ctx context.Context, namespace, name string, body map[string]any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode Kubernetes Secret: %w", err)
	}
	_, err = c.do(ctx, http.MethodPut, namespace, name, encoded)
	return err
}

// do performs one authenticated call with the service account token, reading token and CA per request.
func (c *Client) do(ctx context.Context, method, namespace, name string, body []byte) ([]byte, error) {
	token, err := os.ReadFile(filepath.Join(c.serviceAccount, "token"))
	if err != nil {
		return nil, fmt.Errorf("read service account token: %w", err)
	}
	transport, err := c.roundTripper()
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+secretPath(namespace, name), reader)
	if err != nil {
		return nil, fmt.Errorf("build Kubernetes Secret %s request: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+jscompat.Trim(string(token)))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := noRedirect(&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return nil, err
	}
	raw, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, fmt.Errorf("read Kubernetes Secret %s response: %w", method, err)
	}
	if response.StatusCode == http.StatusConflict {
		return nil, mint.ErrSecretConflict
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("Kubernetes Secret %s failed with HTTP %d", method, response.StatusCode) //nolint:staticcheck // ST1005: user-facing message, text is fixed.
	}
	return raw, nil
}

// noRedirect returns a shallow copy of client that never follows a redirect. The service
// account token is sent on every request, and Go would resend it on a same-host redirect.
func noRedirect(client *http.Client) *http.Client {
	inner := *client
	inner.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &inner
}

// roundTripper returns the injected test transport, or one that trusts the service account CA.
func (c *Client) roundTripper() (http.RoundTripper, error) {
	if c.transport != nil {
		return c.transport, nil
	}
	ca, err := os.ReadFile(filepath.Join(c.serviceAccount, "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("read service account CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("service account CA bundle has no certificates")
	}
	return &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}, nil
}
