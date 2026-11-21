// SPDX-License-Identifier: MIT OR Apache-2.0

// Package kubeserviceaccount reads a Kubernetes step pod's service account and
// builds an authenticated client for the in-cluster API server. The release
// queue store in cdrelease uses it.
package kubeserviceaccount

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Service-account files the kubelet mounts into every step pod.
const (
	TokenPath     = "/var/run/secrets/kubernetes.io/serviceaccount/token" //nolint:gosec // G101 flags the name, not a credential.
	NamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	CAPath        = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

// requestTimeout bounds one API call. The caller's context carries any larger
// deadline; this timeout bounds the rest.
const requestTimeout = 10 * time.Second

// ErrNotKubernetes means KUBERNETES_SERVICE_HOST is unset: the process is not
// running in a cluster.
var ErrNotKubernetes = errors.New("not running in Kubernetes")

// Env reads one environment variable. Tests pass a map.
type Env func(string) string

// ReadFile reads one service-account file. Tests inject one.
type ReadFile func(path string) ([]byte, error)

// StatusError is an API response that was not 2xx.
type StatusError struct {
	Status int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("kubernetes API returned %d", e.Status)
}

// NotFound reports whether err is a 404 from the API.
func NotFound(err error) bool {
	status, ok := errors.AsType[*StatusError](err)
	return ok && status.Status == http.StatusNotFound
}

// Conflict reports whether err is a 409 from the API: another writer changed the
// object first, so the caller re-reads.
func Conflict(err error) bool {
	status, ok := errors.AsType[*StatusError](err)
	return ok && status.Status == http.StatusConflict
}

// Options are the injectable seams of Connect. The zero value reads the real
// service-account files and dials the in-cluster API.
type Options struct {
	// Env reads the environment. Nil means os.Getenv.
	Env Env
	// Read reads a service-account file. Nil means os.ReadFile.
	Read ReadFile
	// HTTP is the client that sends requests. Nil builds one that trusts only the
	// mounted CA.
	HTTP *http.Client
	// Base overrides the API base URL (scheme, host and port). Empty means the
	// in-cluster address from KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT.
	Base string
}

// Cluster is an authenticated client for one namespace of the API server. The
// bearer token is re-read on every request because the kubelet rotates it.
type Cluster struct {
	base      string
	namespace string
	read      ReadFile
	http      *http.Client
}

// Connect builds a Cluster from the step pod's environment and service-account
// files. It returns ErrNotKubernetes when KUBERNETES_SERVICE_HOST is unset and
// no Base is injected. It fails at setup when a required file or variable is
// missing or empty.
func Connect(opts Options) (*Cluster, error) {
	env := opts.Env
	if env == nil {
		env = os.Getenv
	}
	read := opts.Read
	if read == nil {
		read = os.ReadFile
	}
	base := opts.Base
	if base == "" {
		host := strings.TrimSpace(env("KUBERNETES_SERVICE_HOST"))
		if host == "" {
			return nil, ErrNotKubernetes
		}
		port, err := requiredEnv(env, "KUBERNETES_SERVICE_PORT")
		if err != nil {
			return nil, err
		}
		base = "https://" + net.JoinHostPort(host, port)
	}
	if _, err := readRequired(read, TokenPath, "token"); err != nil {
		return nil, fmt.Errorf("%w (check the agent sets WOODPECKER_BACKEND_K8S_SERVICE_ACCOUNT_NAME_ALLOW_FROM_STEP=true and the step's service account automounts its token)", err)
	}
	namespace, err := readRequired(read, NamespacePath, "namespace")
	if err != nil {
		return nil, err
	}
	ca, err := readRequired(read, CAPath, "CA certificate")
	if err != nil {
		return nil, err
	}

	httpClient := opts.HTTP
	if httpClient == nil {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(ca)) {
			return nil, errors.New("cannot read Kubernetes service-account CA certificate: no PEM certificate")
		}
		httpClient = &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		}}
	}
	return &Cluster{base: base, namespace: namespace, read: read, http: noRedirect(httpClient)}, nil
}

// noRedirect returns a shallow copy of client that never follows a redirect. Go
// keeps Authorization on a same-host redirect to another port, so following one
// would resend the service-account token. The 3xx then surfaces as a
// StatusError. The copy leaves the caller's client unchanged.
func noRedirect(client *http.Client) *http.Client {
	inner := *client
	inner.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &inner
}

// Namespace returns the namespace the step pod runs in.
func (c *Cluster) Namespace() string { return c.namespace }

// Do sends one request to the API path under the Cluster's base, with the bearer
// token, and decodes the JSON response into out (which may be nil). body, when
// non-nil, is sent as JSON. Non-2xx responses return a *StatusError and never
// carry the token or the response body. A failed body Close is joined into the
// returned error, not dropped.
func (c *Cluster) Do(ctx context.Context, method, path string, body, out any) (err error) {
	token, err := readRequired(c.read, TokenPath, "token")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, resp.Body.Close()) }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &StatusError{Status: resp.StatusCode}
	}
	if out == nil {
		return nil
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read kubernetes response: %w", err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode kubernetes response: %w", err)
	}
	return nil
}

// EscapePath escapes one path segment, such as an object name.
func EscapePath(segment string) string { return url.PathEscape(segment) }

func readRequired(read ReadFile, path, label string) (string, error) {
	raw, err := read(path)
	if err != nil {
		return "", fmt.Errorf("cannot read Kubernetes service-account %s: %w", label, err)
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return "", fmt.Errorf("cannot read Kubernetes service-account %s: empty file", label)
	}
	return value, nil
}

func requiredEnv(env Env, name string) (string, error) {
	value := strings.TrimSpace(env(name))
	if value == "" {
		return "", fmt.Errorf("missing %s", name)
	}
	return value, nil
}
