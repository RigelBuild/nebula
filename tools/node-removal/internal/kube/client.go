// Package kube is the apiserver transport the node-removal reconciler uses.
package kube

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"strings"
)

const serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// Response is one apiserver answer with its body read in full.
type Response struct {
	Status int
	Body   []byte
}

// OK reports a 2xx status.
func (r Response) OK() bool {
	return r.Status >= 200 && r.Status < 300
}

// Client is the apiserver surface the reconciler needs. Tests inject a fake.
type Client interface {
	Get(path string) (Response, error)
	Patch(path string, body any) (Response, error)
	Post(path string, body any) (Response, error)
	Delete(path string, body any) (Response, error)
}

type inClusterClient struct {
	base string
	fsys fs.FS
	http *http.Client
}

// NewInClusterClient reads the service-account CA once, then sends each request with the
// current token. A missing env var or a bad CA returns an error, and the caller aborts.
func NewInClusterClient(getenv func(string) string) (Client, error) {
	return newInClusterClient(getenv, os.DirFS(serviceAccountDir))
}

// newInClusterClient is NewInClusterClient with the service-account directory injected.
func newInClusterClient(getenv func(string) string, fsys fs.FS) (Client, error) {
	host, port := getenv("KUBERNETES_SERVICE_HOST"), getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("Kubernetes service environment is missing") //nolint:staticcheck // ST1005: user-facing message, text is fixed.
	}
	ca, err := fs.ReadFile(fsys, "ca.crt")
	if err != nil {
		return nil, fmt.Errorf("reading service account CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("reading service account CA: no certificates found")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	return &inClusterClient{
		base: "https://" + net.JoinHostPort(host, port),
		fsys: fsys,
		http: &http.Client{Transport: transport},
	}, nil
}

func (c *inClusterClient) Get(path string) (Response, error) {
	return c.do(http.MethodGet, path, nil)
}

func (c *inClusterClient) Patch(path string, body any) (Response, error) {
	return c.do(http.MethodPatch, path, body)
}

func (c *inClusterClient) Post(path string, body any) (Response, error) {
	return c.do(http.MethodPost, path, body)
}

func (c *inClusterClient) Delete(path string, body any) (Response, error) {
	return c.do(http.MethodDelete, path, body)
}

// do sends one request. A body is JSON; POST uses application/json and every other method
// uses merge-patch.
func (c *inClusterClient) do(method, path string, body any) (Response, error) {
	token, err := fs.ReadFile(c.fsys, "token")
	if err != nil {
		return Response{}, fmt.Errorf("reading service account token: %w", err)
	}
	var payload io.Reader
	contentType := ""
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return Response{}, fmt.Errorf("encoding %s %s body: %w", method, path, err)
		}
		payload = bytes.NewReader(encoded)
		contentType = "application/merge-patch+json"
		if method == http.MethodPost {
			contentType = "application/json"
		}
	}
	request, err := http.NewRequest(method, c.base+path, payload)
	if err != nil {
		return Response{}, fmt.Errorf("building %s %s: %w", method, path, err)
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return Response{}, fmt.Errorf("%s %s: %w", method, path, err)
	}
	raw, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if joined := errors.Join(readErr, closeErr); joined != nil {
		return Response{}, fmt.Errorf("reading %s %s: %w", method, path, joined)
	}
	return Response{Status: response.StatusCode, Body: raw}, nil
}
