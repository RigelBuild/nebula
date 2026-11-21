package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/RigelBuild/nebula/tools/go-lib/cdgate"
)

// manualRelease is one manual main pipeline row carrying a release dispatch id.
func manualRelease(number int64, dispatchID, sha string) map[string]any {
	return map[string]any{
		"number":    number,
		"commit":    sha,
		"branch":    "main",
		"event":     "manual",
		"variables": map[string]string{cdgate.DispatchIDVariable: dispatchID},
	}
}

// newReleaseServer answers the repo lookup and the manual main pipeline list as
// Woodpecker would. It counts the manual-list reads, so a test can see whether
// the publish fence ran. Any other path is a 404.
func newReleaseServer(t *testing.T, manual []map[string]any) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	reads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/repos/lookup/owner/repo":
			if _, err := io.WriteString(w, `{"id":7}`); err != nil {
				t.Errorf("write repo: %v", err)
			}
		case r.URL.Path == "/api/repos/7/pipelines" && r.URL.Query().Get("event") == "manual":
			mu.Lock()
			reads++
			mu.Unlock()
			page := []map[string]any{}
			if r.URL.Query().Get("page") == "1" {
				page = manual
			}
			if err := json.NewEncoder(w).Encode(page); err != nil {
				t.Errorf("encode pipelines: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() int {
		mu.Lock()
		defer mu.Unlock()
		return reads
	}
}

// TestPublishFenceExitsSeventyNineWhenNotNewestRelease: pipeline 10 carries the
// dispatch d10, but pipeline 11 is a newer release. The publish must not run and
// must exit 79, so Woodpecker ends the step skipped.
func TestPublishFenceExitsSeventyNineWhenNotNewestRelease(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	srv, manualReads := newReleaseServer(t, []map[string]any{
		manualRelease(11, "d11", sha),
		manualRelease(10, "d10", sha),
	})
	marker := filepath.Join(t.TempDir(), "published")
	env := map[string]string{
		"WOODPECKER_API_URL":      srv.URL,
		"WOODPECKER_CD_TOKEN":     "tok",
		"CI_REPO":                 "owner/repo",
		"CI_PIPELINE_NUMBER":      "10",
		"CI_PIPELINE_PARENT":      "0",
		"CI_COMMIT_SHA":           sha,
		cdgate.DispatchIDVariable: "d10",
	}
	t.Chdir(t.TempDir())
	var stderr bytes.Buffer
	code := run(t.Context(),
		[]string{"ci:publish", "--members", "publish-ci", "--in-cluster-url", "http://ci.example.com", "--pulumi-root", "stacks", "--pulumi-tiers", "platform", "--", "touch", marker},
		func(name string) string { return env[name] }, &stderr)
	if code != cdgate.ReleaseFenceExitCode {
		t.Fatalf("run = %d, want %d for a publish that is not the newest release; stderr=%q",
			code, cdgate.ReleaseFenceExitCode, stderr.String())
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the publish ran although pipeline 10 is not the newest release")
	}
	if !strings.Contains(stderr.String(), "not the newest release") {
		t.Fatalf("stderr = %q, want the not-the-newest-release line", stderr.String())
	}
	if got := manualReads(); got != 1 {
		t.Fatalf("manual pipeline reads = %d, want 1: the fence stops at the first release", got)
	}
}

// TestPublishFenceSkipsStepOutsideAReleasePipeline: a step with no
// PETREL_CD_DISPATCH_ID is not part of a release, so the fence never queries the
// manual list. This keeps the non-release run-cd path unchanged. The gate's own
// pipeline read is a 404 here, and the gate fails open, so the command runs.
func TestPublishFenceSkipsStepOutsideAReleasePipeline(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	srv, manualReads := newReleaseServer(t, []map[string]any{manualRelease(11, "d11", sha)})
	marker := filepath.Join(t.TempDir(), "published")
	env := map[string]string{
		"WOODPECKER_API_URL":  srv.URL,
		"WOODPECKER_CD_TOKEN": "tok",
		"CI_REPO":             "owner/repo",
		"CI_PIPELINE_NUMBER":  "10",
		"CI_PIPELINE_PARENT":  "0",
		"CI_COMMIT_SHA":       sha,
	}
	t.Chdir(t.TempDir())
	var stderr bytes.Buffer
	code := run(t.Context(),
		[]string{"ci:publish", "--members", "publish-ci", "--in-cluster-url", "http://ci.example.com", "--pulumi-root", "stacks", "--pulumi-tiers", "platform", "--", "touch", marker},
		func(name string) string { return env[name] }, &stderr)
	if code != 0 {
		t.Fatalf("run = %d, want 0 for a step outside a release; stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the step outside a release did not run: %v", err)
	}
	if got := manualReads(); got != 0 {
		t.Fatalf("manual pipeline reads = %d, want 0 outside a release", got)
	}
}
