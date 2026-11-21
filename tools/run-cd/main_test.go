package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
