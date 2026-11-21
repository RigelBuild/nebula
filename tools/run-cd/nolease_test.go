package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
)

// TestRunWithoutLeaseFlagTakesNoLeaseOnKubernetes: with the Kubernetes host set
// but no --lease flag, the run takes no Lease. The Woodpecker server answers the
// gate's reads; the Kubernetes host is unreachable, so any Lease call would fail
// the step. The step must succeed and the server must see only pipeline reads.
func TestRunWithoutLeaseFlagTakesNoLeaseOnKubernetes(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Path
		if r.URL.RawQuery != "" {
			key += "?" + r.URL.RawQuery
		}
		mu.Lock()
		seen = append(seen, key)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch key {
		case "/api/repos/lookup/owner/repo":
			fmt.Fprint(w, `{"id":7}`)
		case "/api/repos/7/pipelines?branch=main&event=push%2Cmanual&page=1&perPage=50":
			fmt.Fprint(w, `[]`)
		case "/api/repos/7/pipelines/20":
			fmt.Fprintf(w, `{"number":20,"commit":%q,"branch":"main","event":"push"}`, sha)
		default:
			t.Errorf("unexpected request %q", key)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	env := map[string]string{
		"WOODPECKER_API_URL":      srv.URL,
		"WOODPECKER_CD_TOKEN":     "tok",
		"CI_REPO":                 "owner/repo",
		"CI_PIPELINE_NUMBER":      "20",
		"CI_PIPELINE_PARENT":      "0",
		"CI_COMMIT_SHA":           sha,
		"KUBERNETES_SERVICE_HOST": "127.0.0.1",
		"KUBERNETES_SERVICE_PORT": "1",
	}
	getenv := func(key string) string { return env[key] }

	workingDir := t.TempDir()
	writePulumiStack(t, workingDir, "aws")
	t.Chdir(workingDir)
	var stderr bytes.Buffer
	code := run(context.Background(),
		[]string{"aws:up", "--members", "pulumi-up-aws", "--in-cluster-url", "http://ci.example.com", "--pulumi-root", "stacks", "--pulumi-tiers", "platform", "--", "true"},
		getenv, &stderr)
	if code != 0 {
		t.Fatalf("run = %d, want 0: with no --lease flag the step must not touch the Lease API; stderr=%q", code, stderr.String())
	}
	mu.Lock()
	defer mu.Unlock()
	allowed := map[string]bool{
		"/api/repos/lookup/owner/repo":                                             true,
		"/api/repos/7/pipelines?branch=main&event=push%2Cmanual&page=1&perPage=50": true,
		"/api/repos/7/pipelines/20":                                                true,
	}
	for _, req := range seen {
		if !allowed[req] {
			t.Fatalf("unexpected request %q: only the repo lookup and pipeline reads are allowed", req)
		}
	}
	if !slices.Contains(seen, "/api/repos/lookup/owner/repo") {
		t.Fatalf("the gate never resolved the repo; requests = %v", seen)
	}
}
