package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/cdgate"
)

// withDeploy fills the deployment inputs with synthetic values.
func withDeploy(opts options) options {
	opts.inClusterURL = "http://ci.example.com"
	opts.pulumiRoot = "stacks"
	opts.pulumiTiers = []string{"platform"}
	return opts
}

// mustNewRun builds the run for opts with the synthetic deployment inputs.
func mustNewRun(t *testing.T, opts options, getenv func(string) string, stderr io.Writer) cdgate.Run {
	t.Helper()
	r, err := newRun(withDeploy(opts), getenv, stderr)
	if err != nil {
		t.Fatalf("newRun: %v", err)
	}
	return r
}

func writePulumiStack(t *testing.T, root, project string) {
	t.Helper()
	stackDir := filepath.Join(root, "stacks", "platform", project)
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stackDir, "Pulumi.yaml"), []byte("name: "+project), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRunGitBoundsAHungGit puts a git that sleeps on PATH and asserts the
// deadline ends the read well before the sleep finishes. Without the bound the
// call would wait out the sleep and return success.
func TestRunGitBoundsAHungGit(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "git")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nsleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	start := time.Now()
	_, err := runGit(context.Background(), 100*time.Millisecond, "rev-parse", "HEAD")
	if err == nil {
		t.Fatal("runGit returned success from a git that outlived its deadline")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("runGit took %v; the 100ms deadline did not bound the hung git", elapsed)
	}
}

func TestParseArgsAcceptsTargetMembersAndCommand(t *testing.T) {
	opts, err := parseArgs([]string{"aws:up", "--members", "pulumi-up-aws,test", "--in-cluster-url", "http://ci.example.com", "--pulumi-root", "stacks", "--pulumi-tiers", "platform", "--", "pulumi", "up"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if opts.target != "aws:up" || opts.lease != "" {
		t.Fatalf("opts = %+v", opts)
	}
	if strings.Join(opts.members, ",") != "pulumi-up-aws,test" {
		t.Fatalf("members = %v", opts.members)
	}
	if strings.Join(opts.command, " ") != "pulumi up" {
		t.Fatalf("command = %v", opts.command)
	}
}

func TestParseArgsReadsLeaseFlag(t *testing.T) {
	opts, err := parseArgs([]string{"aws:up", "--lease", "cd-lease", "--members", "pulumi-up-aws", "--in-cluster-url", "http://ci.example.com", "--pulumi-root", "stacks", "--pulumi-tiers", "platform", "--", "pulumi", "up"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if opts.lease != "cd-lease" {
		t.Fatalf("lease = %q, want cd-lease", opts.lease)
	}
}

func TestParseArgsRejectsUsageErrors(t *testing.T) {
	cases := map[string][]string{
		"no args":            {},
		"no separator":       {"aws:up", "--members", "x"},
		"no members":         {"aws:up", "--", "pulumi", "up"},
		"empty members":      {"aws:up", "--members", ",", "--in-cluster-url", "http://ci.example.com", "--pulumi-root", "stacks", "--pulumi-tiers", "platform", "--", "pulumi"},
		"no command":         {"aws:up", "--members", "x", "--"},
		"lease without name": {"aws:up", "--lease", "", "--members", "x", "--in-cluster-url", "http://ci.example.com", "--pulumi-root", "stacks", "--pulumi-tiers", "platform", "--", "pulumi"},
		"unknown flag":       {"aws:up", "--bogus", "--members", "x", "--in-cluster-url", "http://ci.example.com", "--pulumi-root", "stacks", "--pulumi-tiers", "platform", "--", "pulumi"},
		"empty target":       {"", "--members", "x", "--in-cluster-url", "http://ci.example.com", "--pulumi-root", "stacks", "--pulumi-tiers", "platform", "--", "pulumi"},
	}
	for name, args := range cases {
		if _, err := parseArgs(args); err == nil {
			t.Fatalf("%s: parseArgs(%q) accepted a usage error", name, args)
		}
	}
}

func TestParseArgsRequiresDeploymentInputs(t *testing.T) {
	full := map[string]string{
		"--in-cluster-url": "http://ci.example.com",
		"--pulumi-root":    "stacks",
		"--pulumi-tiers":   "platform",
	}
	for missing := range full {
		t.Run(missing, func(t *testing.T) {
			args := []string{"aws:up", "--members", "x"}
			for flag, value := range full {
				if flag != missing {
					args = append(args, flag, value)
				}
			}
			args = append(args, "--", "true")
			if _, err := parseArgs(args); err == nil {
				t.Fatalf("parseArgs(%q) accepted a run without %s", args, missing)
			}
		})
	}
}

func TestNewRunRejectsEmptyPulumiInputs(t *testing.T) {
	cases := map[string]options{
		"pulumi root":  {inClusterURL: "http://ci.example.com", pulumiTiers: []string{"platform"}},
		"pulumi tiers": {inClusterURL: "http://ci.example.com", pulumiRoot: "stacks"},
	}
	for name, opts := range cases {
		if _, err := newRun(opts, func(string) string { return "" }, io.Discard); err == nil {
			t.Fatalf("%s: newRun accepted an empty input", name)
		}
	}
}

func TestRunUsageErrorExitsTwo(t *testing.T) {
	var stderr bytes.Buffer
	code := run(context.Background(), []string{"aws:up"}, func(string) string { return "" }, &stderr)
	if code != 2 {
		t.Fatalf("run = %d, want 2 for a usage error", code)
	}
	if !strings.Contains(stderr.String(), "usage: run-cd") {
		t.Fatalf("stderr = %q, want the usage line", stderr.String())
	}
}

func TestRunMissingCDTokenBlocksDeploy(t *testing.T) {
	// The API URL is set but the CD token is not. The gate cannot build its API
	// context, so it blocks: exit 1 and the deploy never runs. The deploy is a
	// `touch` of a marker file, and the marker must not appear.
	marker := filepath.Join(t.TempDir(), "deployed")
	env := map[string]string{
		"WOODPECKER_API_URL": "https://ci.example",
		"CI_REPO_ID":         "42",
		"CI_PIPELINE_NUMBER": "12",
		"CI_PIPELINE_PARENT": "0",
		"CI_COMMIT_SHA":      strings.Repeat("b", 40),
	}
	var stderr bytes.Buffer
	code := run(context.Background(),
		[]string{"aws:up", "--members", "pulumi-up-aws", "--in-cluster-url", "http://ci.example.com", "--pulumi-root", "stacks", "--pulumi-tiers", "platform", "--", "touch", marker},
		func(name string) string { return env[name] }, &stderr)
	if code != 1 {
		t.Fatalf("run = %d, want 1: a missing CD token must block the deploy", code)
	}
	if !strings.Contains(stderr.String(), "cd-gate: blocked") {
		t.Fatalf("stderr = %q, want the cd-gate: blocked line", stderr.String())
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the deploy ran: its marker file exists")
	}
}

func TestRunCDRetriesGateSetupAfterRepoLookupFailure(t *testing.T) {
	var lookups int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/repos/lookup/owner/repo":
			lookups++
			if lookups == 1 {
				http.Error(w, "temporary", http.StatusServiceUnavailable)
				return
			}
			if _, err := io.WriteString(w, `{"id":7}`); err != nil {
				t.Errorf("write repo: %v", err)
			}
		case "/api/repos/7/pipelines/12":
			body := `{"number":12,"commit":"` + strings.Repeat("b", 40) + `","branch":"main","event":"push","workflows":[{"name":"pulumi-up-aws","state":"running"}]}`
			if _, err := io.WriteString(w, body); err != nil {
				t.Errorf("write own pipeline: %v", err)
			}
		case "/api/repos/7/pipelines":
			if _, err := io.WriteString(w, `[]`); err != nil {
				t.Errorf("write pipeline list: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	gitDir := t.TempDir()
	git := filepath.Join(gitDir, "git")
	gitScript := "#!/bin/sh\ncase \"$1\" in\nrev-parse) printf 'false\\n' ;;\nrev-list) printf '%s\\n%s\\n' " + strings.Repeat("a", 40) + " " + strings.Repeat("b", 40) + " ;;\nesac\n"
	if err := os.WriteFile(git, []byte(gitScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", gitDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	env := map[string]string{
		"WOODPECKER_API_URL":  "http://" + server.Listener.Addr().String(),
		"WOODPECKER_CD_TOKEN": "token",
		"CI_REPO":             "owner/repo",
		"CI_PIPELINE_NUMBER":  "12",
		"CI_PIPELINE_PARENT":  "0",
		"CI_COMMIT_SHA":       strings.Repeat("b", 40),
	}
	getenv := func(name string) string { return env[name] }
	workingDir := t.TempDir()
	writePulumiStack(t, workingDir, "aws")
	t.Chdir(workingDir)
	r := mustNewRun(t, options{target: "aws:up", members: []string{"pulumi-up-aws"}}, getenv, io.Discard)
	ctx := context.Background()
	if _, err := r.Gate(ctx); err == nil {
		t.Fatal("first Gate call succeeded despite the repository lookup failure")
	}
	if lookups != 1 {
		t.Fatalf("repo lookups after first Gate call = %d, want 1", lookups)
	}
	if _, err := r.Gate(ctx); err != nil {
		t.Fatalf("second Gate call: %v", err)
	}
	if lookups != 2 {
		t.Fatalf("repo lookups after retry = %d, want 2", lookups)
	}
	if _, err := r.Gate(ctx); err != nil {
		t.Fatalf("third Gate call: %v", err)
	}
	if lookups != 2 {
		t.Fatalf("repo lookups after cached success = %d, want 2", lookups)
	}
}

// TestRunInClusterURLReachesPipelineAPIOnKubernetes runs a publish step on a
// Kubernetes host with no WOODPECKER_API_URL, so --in-cluster-url is the only
// Woodpecker address. Both call sites must use it: newestReleaseFence reads the
// manual list, and the gate's setup in newRun fetches the own pipeline. With an
// empty URL the fence fails at once, or the gate blocks on ErrInvalidAPIContext
// before its pipeline fetch. Either way run exits 1 and the deploy never runs.
func TestRunInClusterURLReachesPipelineAPIOnKubernetes(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	manual, err := json.Marshal([]map[string]any{manualRelease(12, "d12", sha)})
	if err != nil {
		t.Fatal(err)
	}
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
		var body []byte
		switch key {
		case "/api/repos/lookup/owner/repo":
			body = []byte(`{"id":7}`)
		case "/api/repos/7/pipelines?branch=main&event=manual&page=1&perPage=50":
			body = manual
		case "/api/repos/7/pipelines?branch=main&event=push%2Cmanual&page=1&perPage=50":
			body = []byte(`[]`)
		case "/api/repos/7/pipelines/12":
			body = []byte(`{"number":12,"commit":"` + sha + `","branch":"main","event":"manual","workflows":[{"name":"pulumi-up-aws","state":"running"}]}`)
		default:
			t.Errorf("unexpected request %q", key)
			http.NotFound(w, r)
			return
		}
		if _, err := w.Write(body); err != nil {
			t.Errorf("write %s: %v", key, err)
		}
	}))
	t.Cleanup(srv.Close)

	// runMain passes os.Getenv, so the test sets the environment with t.Setenv
	// and reads it the same way. WOODPECKER_API_URL is cleared so the in-cluster
	// URL is the only source, and CI_REPO_ID is cleared so the repo lookup runs.
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	t.Setenv("WOODPECKER_API_URL", "")
	t.Setenv("CI_REPO_ID", "")
	t.Setenv("WOODPECKER_CD_TOKEN", "tok")
	t.Setenv("CI_REPO", "owner/repo")
	t.Setenv("CI_PIPELINE_NUMBER", "12")
	t.Setenv("CI_PIPELINE_PARENT", "0")
	t.Setenv("CI_COMMIT_SHA", sha)
	t.Setenv(cdgate.DispatchIDVariable, "d12")

	gitDir := t.TempDir()
	gitScript := "#!/bin/sh\ncase \"$1\" in\nrev-parse) printf 'false\\n' ;;\nrev-list) printf '%s\\n' " + sha + " ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(gitDir, "git"), []byte(gitScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", gitDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	workingDir := t.TempDir()
	writePulumiStack(t, workingDir, "aws")
	t.Chdir(workingDir)

	marker := filepath.Join(t.TempDir(), "deployed")
	var stderr bytes.Buffer
	code := run(t.Context(),
		[]string{"aws:up", "--members", "pulumi-up-aws", "--in-cluster-url", srv.URL, "--pulumi-root", "stacks", "--pulumi-tiers", "platform", "--", "touch", marker},
		os.Getenv, &stderr)
	if code != 0 {
		t.Fatalf("run = %d, want 0 with the API reached only through --in-cluster-url; stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the deploy did not run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	// The manual-list read is newestReleaseFence's. The own-pipeline fetch
	// comes only from the gate's setup in newRun.
	for _, want := range []string{
		"/api/repos/7/pipelines?branch=main&event=manual&page=1&perPage=50",
		"/api/repos/7/pipelines/12",
	} {
		if !slices.Contains(seen, want) {
			t.Fatalf("the server never saw %q; requests = %v", want, seen)
		}
	}
}
