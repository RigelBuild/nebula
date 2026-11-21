package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitIn runs git in dir and returns its trimmed output.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestRunningDescendantCoversAndGateSkips: a running pipeline on a descendant
// commit covers the group, so the CLI skips with the superseded line and exits
// 79 (skip_exit_code), which Woodpecker reports as a skipped step.
// The repo is real: an origin with c1 and its descendant c2 on main.
func TestRunningDescendantCoversAndGateSkips(t *testing.T) {
	directory := t.TempDir()
	origin := filepath.Join(directory, "origin.git")
	work := filepath.Join(directory, "work")
	gitIn(t, directory, "init", "-q", "--bare", "-b", "main", origin)
	gitIn(t, directory, "clone", "-q", origin, work)
	gitIn(t, work, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "c1")
	c1 := gitIn(t, work, "rev-parse", "HEAD")
	gitIn(t, work, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "c2")
	c2 := gitIn(t, work, "rev-parse", "HEAD")
	gitIn(t, work, "push", "-q", "origin", "main")
	gitIn(t, work, "checkout", "-q", c1)

	workflows := []map[string]string{
		{"name": "pulumi-up-aws", "state": "running"},
		{"name": "nix-deploy-fleet", "state": "running"},
	}
	own := map[string]any{"number": 10, "created": 100, "commit": c1, "branch": "main", "event": "push"}
	newer := map[string]any{"number": 11, "created": 200, "commit": c2, "branch": "main", "event": "push"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		encode := func(v any) {
			if err := json.NewEncoder(w).Encode(v); err != nil {
				t.Errorf("encode %s: %v", r.URL.Path, err)
			}
		}
		switch r.URL.Path {
		case "/api/repos/lookup/owner/repo":
			fmt.Fprint(w, `{"id":2}`)
		case "/api/repos/2/pipelines":
			if r.URL.Query().Get("page") == "1" {
				encode([]any{newer, own})
				return
			}
			fmt.Fprint(w, `[]`)
		case "/api/repos/2/pipelines/10":
			encode(map[string]any{
				"number": 10, "commit": c1, "branch": "main", "event": "push", "workflows": workflows,
			})
		case "/api/repos/2/pipelines/11":
			encode(map[string]any{
				"number": 11, "commit": c2, "branch": "main", "event": "push", "workflows": workflows,
			})
		default:
			t.Errorf("unexpected request %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	env := map[string]string{
		"WOODPECKER_API_URL":  srv.URL,
		"WOODPECKER_CD_TOKEN": "synthetic-token",
		"CI_REPO":             "owner/repo",
		"CI_PIPELINE_NUMBER":  "10",
		"CI_PIPELINE_PARENT":  "0",
		"CI_COMMIT_SHA":       c1,
	}
	var stderr bytes.Buffer
	if err := os.MkdirAll(filepath.Join(work, "stacks", "platform", "aws"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "stacks", "platform", "aws", "Pulumi.yaml"), []byte("name: aws"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)
	code := run(context.Background(),
		[]string{"aws:up", "--members", "pulumi-up-aws,nix-deploy-fleet", "--in-cluster-url", "http://ci.example.com", "--pulumi-root", "stacks", "--pulumi-tiers", "platform", "--", "true"},
		func(key string) string { return env[key] }, &stderr)
	if code != 79 {
		t.Fatalf("run = %d, want 79 (skip_exit_code) for a superseded skip; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "cd-gate: skipped superseded 11") {
		t.Fatalf("log = %q, want the superseded skip for pipeline 11", stderr.String())
	}
}

// stopExit must fail a stopped step even when it was about to skip (79); an
// unstopped skip keeps 79, and a stopped deploy failure keeps its own code.
func TestStopExitFailsStoppedSkipAndKeepsSkipCode(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	live := context.Background()
	cases := []struct {
		name string
		ctx  context.Context
		code int
		want int
	}{
		{"stopped skip fails", cancelled, 79, 1},
		{"stopped clean run fails", cancelled, 0, 1},
		{"stopped deploy failure keeps its code", cancelled, 5, 5},
		{"unstopped skip stays skip", live, 79, 79},
		{"unstopped clean run stays clean", live, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stopExit(tc.ctx, tc.code); got != tc.want {
				t.Fatalf("stopExit(code=%d) = %d, want %d", tc.code, got, tc.want)
			}
		})
	}
}
