// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/linearauth"
)

const passingLog = "bench-latency:e2e |     load_test.go:364: store load rpc=Commit n=384 p50=37ms p99=148ms\n" +
	"bench-latency:e2e |     load_test.go:365: store load rpc=Put n=100 p50=18.2ms p99=44.0ms\n" +
	"bench-latency:e2e |     load_test.go:366: store load land cycles=10 landed=10 p50=31ms max=52ms interval=1s"

type capturedCall struct {
	command string
	args    []string
	options processOptions
}

type linearFixture struct {
	server         *httptest.Server
	requests       []string
	calls          []capturedCall
	benchmark      processResult
	cotalCode      int
	commentFails   bool
	issueMisses    int
	issueMessage   string
	sleeps         []time.Duration
	logs           []string
	config         nightlyConfig
	deps           nightlyDeps
	tokenRequests  int
	revokeRequests int
	oauthRequests  []string
}

func newLinearFixture(t *testing.T) *linearFixture {
	t.Helper()
	fixture := &linearFixture{benchmark: processResult{stdout: passingLog}}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode GraphQL body: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if request.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		fixture.requests = append(fixture.requests, payload.Query)
		writer.Header().Set("Content-Type", "application/json")
		if strings.Contains(payload.Query, "issue(id:") {
			if fixture.issueMisses > 0 {
				fixture.issueMisses--
				if err := json.NewEncoder(writer).Encode(map[string]any{"errors": []map[string]string{{"message": "Entity not found: Issue"}}, "data": nil}); err != nil {
					t.Errorf("encode GraphQL response: %v", err)
				}
				return
			}
			if fixture.issueMessage != "" {
				if err := json.NewEncoder(writer).Encode(map[string]any{"errors": []map[string]string{{"message": fixture.issueMessage}}, "data": nil}); err != nil {
					t.Errorf("encode GraphQL response: %v", err)
				}
				return
			}
		}
		response := map[string]any{"data": map[string]any{"issue": map[string]string{"id": "issue-uuid"}}}
		if strings.Contains(payload.Query, "commentCreate") {
			success := !fixture.commentFails
			response = map[string]any{"data": map[string]any{"commentCreate": map[string]bool{"success": success}}}
		}
		if err := json.NewEncoder(writer).Encode(response); err != nil {
			t.Errorf("encode GraphQL response: %v", err)
		}
	}))
	t.Cleanup(fixture.server.Close)
	fixture.config = nightlyConfig{
		source:       "/source/example.git",
		clone:        filepath.Join(t.TempDir(), "clone"),
		host:         "example-host",
		task:         "bench-latency:e2e",
		issue:        "EX-1",
		mention:      "@example-owner",
		cotalChannel: "example-channel",
		cotalDir:     t.TempDir(),
		credentials:  linearauth.Credentials{ClientID: "client-id", ClientSecret: "client-secret"},
	}
	fixture.deps = nightlyDeps{
		runner: func(_ context.Context, command string, args []string, options processOptions) (processResult, error) {
			fixture.calls = append(fixture.calls, capturedCall{command: command, args: append([]string(nil), args...), options: options})
			if command == "jj" && len(args) > 0 && args[0] == "log" {
				want := []string{"log", "--no-graph", "-r", "main@origin", "-T", "commit_id"}
				if !reflect.DeepEqual(args, want) {
					t.Errorf("jj log args = %v, want %v", args, want)
				}
				return processResult{stdout: strings.Repeat("a", 40) + "\n"}, nil
			}
			if command == "nix" {
				return fixture.benchmark, nil
			}
			if command == "cotal" {
				return processResult{exitCode: fixture.cotalCode, stderr: "cotal failed"}, nil
			}
			return processResult{}, nil
		},
		linearURL: fixture.server.URL,
		client:    fixture.linearClient(),
		now:       func() time.Time { return time.Date(2026, time.October, 22, 3, 30, 0, 0, time.UTC) },
		sleep: func(_ context.Context, delay time.Duration) error {
			fixture.sleeps = append(fixture.sleeps, delay)
			return nil
		},
		log: func(line string) { fixture.logs = append(fixture.logs, line) },
	}
	return fixture
}

func (f *linearFixture) linearClient() *http.Client {
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if strings.HasSuffix(request.URL.Path, "/oauth/token") {
			f.tokenRequests++
			f.oauthRequests = append(f.oauthRequests, request.URL.Path)
			response := httptest.NewRecorder()
			response.Header().Set("Content-Type", "application/json")
			if _, err := fmt.Fprintln(response, `{"access_token":"test-token"}`); err != nil {
				return nil, err
			}
			return response.Result(), nil
		}
		if strings.HasSuffix(request.URL.Path, "/oauth/revoke") {
			f.revokeRequests++
			f.oauthRequests = append(f.oauthRequests, request.URL.Path)
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header), Request: request}, nil
		}
		clone := request.Clone(request.Context())
		cloneURL, err := url.Parse(f.server.URL)
		if err != nil {
			return nil, err
		}
		clone.URL.Scheme = cloneURL.Scheme
		clone.URL.Host = cloneURL.Host
		clone.URL.Path = "/graphql"
		clone.Host = cloneURL.Host
		return http.DefaultTransport.RoundTrip(clone)
	})
	return &http.Client{Transport: transport}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func testNightlySuccess(t *testing.T) {
	t.Helper()
	fixture := newLinearFixture(t)
	result := runNightly(t.Context(), fixture.config, fixture.deps)
	if result.exitCode != 0 {
		t.Fatalf("runNightly() exit code = %d, failures: %v", result.exitCode, result.failureLines)
	}
	wantMeasurements := measurements{
		rpc: []rpcMeasurement{
			{name: "Commit", n: "384", p50: "37ms", p99: "148ms"},
			{name: "Put", n: "100", p50: "18.2ms", p99: "44.0ms"},
		},
		landCycles: &landCycleMeasurement{cycles: "10", landed: "10", p50: "31ms", max: "52ms", interval: "1s"},
	}
	if !reflect.DeepEqual(result.measurements, wantMeasurements) {
		t.Errorf("measurements = %#v, want %#v", result.measurements, wantMeasurements)
	}
	for _, want := range []string{"aaaaaaaaaaaa", "| Commit | 384 | 37ms | 148ms |", "| Land cycles | 10/10 | 31ms | 52ms | 1s |", "rc=0"} {
		if !strings.Contains(result.commentBody, want) {
			t.Errorf("comment body missing %q:\n%s", want, result.commentBody)
		}
	}
	if len(fixture.requests) != 2 || !strings.Contains(fixture.requests[1], "commentCreate") {
		t.Errorf("Linear requests = %v, want issue lookup and comment", fixture.requests)
	}
	benchmark := findCall(fixture.calls, "nix")
	if benchmark == nil {
		t.Fatal("no benchmark command recorded")
	}
	wantArgs := []string{"run", ".#rbuild", "--", "--hosts", "example-host", "--rev", "main@origin", "--", "moon", "run", "bench-latency:e2e"}
	if !reflect.DeepEqual(benchmark.args, wantArgs) {
		t.Errorf("nix args = %v, want %v", benchmark.args, wantArgs)
	}
	if benchmark.options.cwd != fixture.config.clone || benchmark.options.env["RBUILD_LANE"] != nightlyLane || benchmark.options.timeout != rigelNightlyTimeout {
		t.Errorf("nix options = %+v", benchmark.options)
	}
	assertOAuthLifecycle(t, fixture)
}

func assertOAuthLifecycle(t *testing.T, fixture *linearFixture) {
	t.Helper()
	if fixture.tokenRequests != 1 || fixture.revokeRequests != 1 {
		t.Errorf("OAuth requests: token=%d revoke=%d, want one of each", fixture.tokenRequests, fixture.revokeRequests)
	}
	if !reflect.DeepEqual(fixture.oauthRequests, []string{"/oauth/token", "/oauth/revoke"}) {
		t.Errorf("OAuth request order = %v, want mint then revoke", fixture.oauthRequests)
	}
}

func TestRunNightly(t *testing.T) {
	t.Run("parses real-looking passing measurements and posts the comment", testNightlySuccess)

	t.Run("reports prefixed failure lines", func(t *testing.T) {
		fixture := newLinearFixture(t)
		exceeds := "bench-latency:e2e | load_test.go:367: store load rpc=Put exceeds p99 limit"
		failure := "bench-latency:e2e | --- FAIL: TestLatency (1s)"
		fixture.benchmark = processResult{exitCode: 1, stdout: passingLog + "\n" + exceeds + "\n" + failure}
		result := runNightly(t.Context(), fixture.config, fixture.deps)
		if result.exitCode != 1 || !reflect.DeepEqual(result.failureLines, []string{exceeds, failure}) || !strings.Contains(result.commentBody, exceeds) {
			t.Fatalf("runNightly() = %+v", result)
		}
		if !hasCall(fixture.calls, "cotal") {
			t.Error("failed benchmark did not post to Cotal")
		}
	})

	t.Run("reports why rbuild refused to run", func(t *testing.T) {
		fixture := newLinearFixture(t)
		refusal := "rbuild: gate host example-host is held by lane example until 2026-10-30T08:06:40Z; set RBUILD_LANE to the holding lane or pick another --hosts"
		fixture.benchmark = processResult{exitCode: 1, stderr: refusal + "\n"}
		result := runNightly(t.Context(), fixture.config, fixture.deps)
		if result.exitCode != 1 || !reflect.DeepEqual(result.failureLines, []string{refusal}) || !strings.Contains(result.commentBody, refusal) {
			t.Fatalf("runNightly() = %+v", result)
		}
		if got := cotalMessage(fixture.calls); !strings.Contains(got, refusal) {
			t.Errorf("Cotal message = %q", got)
		}
	})

	t.Run("a failing command with only rbuild status lines reports its exit code", func(t *testing.T) {
		fixture := newLinearFixture(t)
		fixture.benchmark = processResult{exitCode: 2, stderr: "rbuild: gating commit " + strings.Repeat("a", 40) + "\nrbuild: gated commit " + strings.Repeat("a", 40) + " exit=2\n"}
		result := runNightly(t.Context(), fixture.config, fixture.deps)
		if !reflect.DeepEqual(result.failureLines, []string{"benchmark command exited with rc=2"}) {
			t.Fatalf("failure lines = %v", result.failureLines)
		}
	})

	t.Run("fails when a successful command prints no land-cycle measurements", func(t *testing.T) {
		fixture := newLinearFixture(t)
		fixture.benchmark = processResult{stdout: "store load rpc=Get n=1 p50=1ms p99=2ms"}
		result := runNightly(t.Context(), fixture.config, fixture.deps)
		if result.exitCode != 1 || !slices.Contains(result.failureLines, "no store load land-cycle measurements parsed") {
			t.Fatalf("runNightly() = %+v", result)
		}
	})

	t.Run("posts one cotal channel message only when the run fails", func(t *testing.T) {
		passing := newLinearFixture(t)
		runNightly(t.Context(), passing.config, passing.deps)
		if hasCall(passing.calls, "cotal") {
			t.Error("passing run posted a Cotal message")
		}
		failing := newLinearFixture(t)
		failing.benchmark = processResult{exitCode: 1, stdout: "bench-latency:e2e | --- FAIL: TestLatency (1s)"}
		runNightly(t.Context(), failing.config, failing.deps)
		want := "storeload latency nightly FAILED on aaaaaaaaaaaa (rc=1): bench-latency:e2e | --- FAIL: TestLatency (1s) — numbers on EX-1 @example-owner"
		if got := cotalMessage(failing.calls); got != want {
			t.Errorf("Cotal message = %q, want %q", got, want)
		}
		if call := findCall(failing.calls, "cotal"); call == nil || call.options.timeout != cotalTimeout || !reflect.DeepEqual(call.args, []string{"send", "--channel", "example-channel", want, "--space", "main"}) {
			t.Errorf("Cotal call = %+v, want channel send argv and %s timeout", call, cotalTimeout)
		}
	})

	t.Run("revokes the Linear app token when commentCreate fails", func(t *testing.T) {
		fixture := newLinearFixture(t)
		fixture.commentFails = true
		result := runNightly(t.Context(), fixture.config, fixture.deps)
		if result.exitCode != 1 || !hasCall(fixture.calls, "cotal") {
			t.Fatalf("runNightly() = %+v", result)
		}
		assertOAuthLifecycle(t, fixture)
	})

	t.Run("retries Entity not found twice on the minted token, then posts", func(t *testing.T) {
		fixture := newLinearFixture(t)
		fixture.issueMisses = 2
		result := runNightly(t.Context(), fixture.config, fixture.deps)
		if result.exitCode != 0 || !reflect.DeepEqual(fixture.sleeps, []time.Duration{2 * time.Second, 4 * time.Second}) {
			t.Fatalf("runNightly() = %+v, sleeps %v", result, fixture.sleeps)
		}
		assertOAuthLifecycle(t, fixture)
		wantLogs := []string{
			"Linear issue lookup attempt 1/3 failed: Entity not found: Issue; retrying in 2s",
			"Linear issue lookup attempt 2/3 failed: Entity not found: Issue; retrying in 4s",
		}
		if !reflect.DeepEqual(fixture.logs, wantLogs) {
			t.Errorf("logs = %v, want %v", fixture.logs, wantLogs)
		}
	})

	t.Run("fails loud after three Entity not found lookups", func(t *testing.T) {
		fixture := newLinearFixture(t)
		fixture.issueMisses = 3
		result := runNightly(t.Context(), fixture.config, fixture.deps)
		if result.exitCode != 1 || !reflect.DeepEqual(fixture.sleeps, []time.Duration{2 * time.Second, 4 * time.Second}) || !slices.Contains(result.failureLines, "Linear report failed: Entity not found: Issue") {
			t.Fatalf("runNightly() = %+v, sleeps %v", result, fixture.sleeps)
		}
	})

	t.Run("does not retry other Linear errors", func(t *testing.T) {
		fixture := newLinearFixture(t)
		fixture.issueMessage = "Authentication required, not authenticated"
		result := runNightly(t.Context(), fixture.config, fixture.deps)
		if result.exitCode != 1 || len(fixture.sleeps) != 0 || !slices.Contains(result.failureLines, "Linear report failed: Authentication required, not authenticated") {
			t.Fatalf("runNightly() = %+v, sleeps %v", result, fixture.sleeps)
		}
	})
}

func hasCall(calls []capturedCall, command string) bool {
	return findCall(calls, command) != nil
}

func findCall(calls []capturedCall, command string) *capturedCall {
	for index := range calls {
		if calls[index].command == command {
			return &calls[index]
		}
	}
	return nil
}

func cotalMessage(calls []capturedCall) string {
	call := findCall(calls, "cotal")
	if call == nil || len(call.args) < 4 {
		return ""
	}
	return call.args[3]
}
