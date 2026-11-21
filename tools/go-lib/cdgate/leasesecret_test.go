package cdgate

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/RigelBuild/nebula/tools/go-lib/kubeserviceaccount"
)

// secretToken is the service-account token the fixtures serve. The secrecy tests
// check it never appears in an error the step can log.
const secretToken = "sa-token-value-that-must-not-leak"

// tokenFiles serves a complete service account whose token is secretToken.
func tokenFiles(t *testing.T) map[string]string {
	t.Helper()
	files := completeFiles(t)
	files[kubeserviceaccount.TokenPath] = secretToken + "\n"
	return files
}

// TestLeaseClientSendsTokenToAPIOnly: the bearer token reaches the Lease API in
// the Authorization header, and nowhere else. The API sees it; the step does not
// return it in a value it could log.
func TestLeaseClientSendsTokenToAPIOnly(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(w, `{"metadata":{"resourceVersion":"1"},"spec":{}}`); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	client := testLeases(t, srv, tokenFiles(t))
	rec, err := client.Get(context.Background(), "cd-lease")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if gotAuth != "Bearer "+secretToken {
		t.Fatalf("Authorization = %q, want the bearer token sent to the API", gotAuth)
	}
	if rec == nil || strings.Contains(rec.ResourceVersion, secretToken) || strings.Contains(rec.HolderIdentity, secretToken) {
		t.Fatalf("the returned record carries the token: %+v", rec)
	}
}

// rotatingToken is a token file the test can rotate between requests. It is
// safe for the concurrent reads a request makes.
type rotatingToken struct {
	mu    sync.Mutex
	token string
}

func (r *rotatingToken) set(token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.token = token
}

func (r *rotatingToken) read(path string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if path != kubeserviceaccount.TokenPath {
		return []byte("woodpecker"), nil
	}
	return []byte(r.token + "\n"), nil
}

// TestLeaseClientRereadsRotatedToken: the kubelet rotates the projected token, so
// each request reads the file again. The token is rotated between two requests,
// and each request must send the token the file held at that moment.
func TestLeaseClientRereadsRotatedToken(t *testing.T) {
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(w, `{"metadata":{"resourceVersion":"1"},"spec":{}}`); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	files := &rotatingToken{token: "first-token"}
	cluster, err := kubeserviceaccount.Connect(kubeserviceaccount.Options{
		Env:  func(string) string { return "" },
		Read: files.read,
		HTTP: srv.Client(),
		Base: srv.URL,
	})
	if err != nil {
		t.Fatalf("kubeserviceaccount.Connect: %v", err)
	}
	client := &kubeLeases{cluster: cluster}

	if _, err := client.Get(context.Background(), "cd-lease"); err != nil {
		t.Fatalf("first Get: %v", err)
	}
	files.set("rotated-token")
	if _, err := client.Get(context.Background(), "cd-lease"); err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if len(auths) != 2 || auths[0] != "Bearer first-token" || auths[1] != "Bearer rotated-token" {
		t.Fatalf("Authorization headers = %q, want the rotated token on the second request", auths)
	}
}

// TestLeaseClientFailureNeverNamesToken: a failing Lease API yields an error that
// carries only the status. The token must not appear in the error text the step
// logs, even though the request used it.
func TestLeaseClientFailureNeverNamesToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echo the request header back in the body, the way a careless proxy might.
		w.WriteHeader(http.StatusForbidden)
		if _, err := io.WriteString(w, "denied for "+r.Header.Get("Authorization")); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	client := testLeases(t, srv, tokenFiles(t))
	_, err := client.Get(context.Background(), "cd-lease")
	if err == nil {
		t.Fatal("Get succeeded against a 403; want an error")
	}
	if strings.Contains(err.Error(), secretToken) {
		t.Fatalf("error text leaks the token: %v", err)
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("error = %v, want the status code logged", err)
	}
}
