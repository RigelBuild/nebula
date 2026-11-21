package kubeserviceaccount

import (
	"errors"
	"io/fs"
	"net/http"
	"strings"
	"testing"
)

// files serves the three service-account files from a map; a missing path is
// fs.ErrNotExist, the same error the kubelet's mount gives.
func files(m map[string]string) ReadFile {
	return func(path string) ([]byte, error) {
		v, ok := m[path]
		if !ok {
			return nil, fs.ErrNotExist
		}
		return []byte(v), nil
	}
}
func inCluster() Env {
	return func(k string) string {
		switch k {
		case "KUBERNETES_SERVICE_HOST":
			return "10.0.0.1"
		case "KUBERNETES_SERVICE_PORT":
			return "443"
		}
		return ""
	}
}

func TestConnectIsNotKubernetesWithoutServiceHost(t *testing.T) {
	_, err := Connect(Options{Env: func(string) string { return "" }, Read: files(nil)})
	if !errors.Is(err, ErrNotKubernetes) {
		t.Fatalf("err = %v, want ErrNotKubernetes outside a cluster", err)
	}
}

func TestConnectNamesTheMissingToken(t *testing.T) {
	_, err := Connect(Options{
		Env:  inCluster(),
		Read: files(map[string]string{NamespacePath: "ns", CAPath: "x"}),
	})
	if err == nil || !strings.Contains(err.Error(), "ALLOW_FROM_STEP") {
		t.Fatalf("err = %v, want the missing token named with the setup hint", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want the missing file wrapped", err)
	}
}

func TestConnectRejectsEmptyNamespace(t *testing.T) {
	_, err := Connect(Options{
		Env: inCluster(),
		Read: files(map[string]string{
			TokenPath: "t", NamespacePath: "   \n", CAPath: "x",
		}),
	})
	if err == nil || !strings.Contains(err.Error(), "empty file") {
		t.Fatalf("err = %v, want an empty-file error for the namespace", err)
	}
}

// The injected HTTP client stands in for the CA-built TLS client, so this test
// is about the namespace and does not need a parseable CA certificate.
func TestConnectReadsNamespaceTrimmed(t *testing.T) {
	c, err := Connect(Options{
		Env:  inCluster(),
		Read: files(map[string]string{TokenPath: "t", NamespacePath: "woodpecker\n", CAPath: "x"}),
		HTTP: &http.Client{},
		Base: "http://127.0.0.1:1",
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got := c.Namespace(); got != "woodpecker" {
		t.Fatalf("Namespace() = %q, want the trimmed namespace", got)
	}
}

func TestStatusHelpersClassifyTheStatus(t *testing.T) {
	notFound := &StatusError{Status: http.StatusNotFound}
	conflict := &StatusError{Status: http.StatusConflict}
	if !NotFound(notFound) || NotFound(conflict) || NotFound(nil) {
		t.Fatal("NotFound misclassifies a 404, a 409, or nil")
	}
	if !Conflict(conflict) || Conflict(notFound) || Conflict(nil) {
		t.Fatal("Conflict misclassifies a 409, a 404, or nil")
	}
	if !NotFound(errors.Join(errors.New("x"), notFound)) {
		t.Fatal("NotFound does not see a wrapped 404")
	}
}

func TestEscapePathEscapesOneSegment(t *testing.T) {
	if got := EscapePath("a/b c"); got != "a%2Fb%20c" {
		t.Fatalf("EscapePath = %q, want a single escaped segment", got)
	}
}
