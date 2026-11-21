// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/RigelBuild/nebula/tools/go-lib/githubapptoken"
)

const testManagedRepos = `{"active":["alpha","beta"],"unmanaged":["gamma"]}`

var cleanOrgRepos = []orgRepo{
	{name: "alpha"},
	{name: "BETA"},
	{name: "gamma"},
}

func TestInventoryGaps(t *testing.T) {
	tests := []struct {
		name string
		org  []orgRepo
		want []string
	}{
		{name: "reports an undeclared repository", org: append(slices.Clone(cleanOrgRepos), orgRepo{name: "new-repo"}), want: []string{"undeclared repo: new-repo"}},
		{name: "reports a declared repository missing from the org", org: cleanOrgRepos[:2], want: []string{"missing repo: gamma"}},
		{name: "ignores archived repositories", org: append(slices.Clone(cleanOrgRepos), orgRepo{name: "old-repo", archived: true}), want: []string{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := inventoryGaps(test.org, managedRepos{Active: []string{"alpha", "beta"}, Unmanaged: []string{"gamma"}})
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("inventoryGaps() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestRunRepoInventoryWithOptions(t *testing.T) {
	tests := []struct {
		name       string
		mint       func(context.Context) (githubapptoken.Token, error)
		handler    http.HandlerFunc
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{
			name: "follows Link pagination and compares all active repositories",
			mint: func(context.Context) (githubapptoken.Token, error) { return "test-token", nil },
			handler: func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodGet || request.Header.Get("Accept") != "application/vnd.github+json" || request.Header.Get("Authorization") != "Bearer test-token" {
					t.Errorf("request = %s headers %v", request.Method, request.Header)
				}
				if request.URL.RawQuery == "per_page=100" {
					writer.Header().Set("Link", `<https://api.github.com/orgs/RigelBuild/repos?page=2>; rel="next"`)
					_, err := io.WriteString(writer, `[{"name":"alpha","archived":false},{"name":"BETA","archived":false}]`)
					if err != nil {
						t.Errorf("write first page: %v", err)
					}
					return
				}
				if request.URL.RawQuery != "page=2" {
					t.Errorf("page query = %q", request.URL.RawQuery)
				}
				_, err := io.WriteString(writer, `[{"name":"gamma","archived":false},{"name":"archived","archived":true}]`)
				if err != nil {
					t.Errorf("write second page: %v", err)
				}
			},
			wantCode: 0, wantStdout: "repo-inventory: 3 repositories match\n",
		},
		{
			name: "returns 2 without echoing the mint error",
			mint: func(context.Context) (githubapptoken.Token, error) {
				return "", errors.New("PRIVATE-KEY-SENTINEL ghs_TOKENSENTINEL")
			},
			wantCode: 2, wantStderr: "repo-inventory: cannot mint installation token\n",
		},
		{
			name: "returns 1 and reports each inventory gap",
			mint: func(context.Context) (githubapptoken.Token, error) { return "test-token", nil },
			handler: func(writer http.ResponseWriter, _ *http.Request) {
				_, err := io.WriteString(writer, `[{"name":"alpha","archived":false},{"name":"BETA","archived":false},{"name":"gamma","archived":false},{"name":"stray","archived":false}]`)
				if err != nil {
					t.Errorf("write response: %v", err)
				}
			},
			wantCode: 1, wantStderr: "repo-inventory: undeclared repo: stray\n",
		},
		{
			name:     "returns 2 and reports an API error",
			mint:     func(context.Context) (githubapptoken.Token, error) { return "test-token", nil },
			handler:  func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusServiceUnavailable) },
			wantCode: 2, wantStderr: "repo-inventory: request failed: HTTP 503\n",
		},
		{
			name: "rejects cross-origin pagination links without following them",
			mint: func(context.Context) (githubapptoken.Token, error) { return "test-token", nil },
			handler: func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Link", `<https://attacker.example/repos?page=2>; rel="next"`)
				_, err := io.WriteString(writer, `[{"name":"alpha","archived":false}]`)
				if err != nil {
					t.Errorf("write response: %v", err)
				}
			},
			wantCode: 2, wantStderr: "repo-inventory: invalid pagination URL: pagination URL must use the GitHub API origin\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var server *httptest.Server
			client := http.DefaultClient
			if test.handler != nil {
				server = httptest.NewServer(test.handler)
				t.Cleanup(server.Close)
				client = &http.Client{Transport: testRoundTripper{base: server.Client().Transport, url: server.URL}}
			}
			stdout := &strings.Builder{}
			stderr := &strings.Builder{}
			code := runRepoInventoryWithOptions(t.Context(), repoInventoryOptions{
				reposText: testManagedRepos,
				mint:      test.mint,
				client:    client,
				stdout:    stdout,
				stderr:    stderr,
			})
			if code != test.wantCode || stdout.String() != test.wantStdout || stderr.String() != test.wantStderr {
				t.Fatalf("runRepoInventoryWithOptions() = (%d, %q, %q), want (%d, %q, %q)", code, stdout.String(), stderr.String(), test.wantCode, test.wantStdout, test.wantStderr)
			}
			if strings.Contains(stderr.String(), "SENTINEL") {
				t.Fatalf("sensitive mint error was echoed: %q", stderr.String())
			}
		})
	}
}

func TestRepoInventoryWriteFailureExitsOperationalError(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		healthyStatus int
		failStdout    bool
	}{
		{name: "complete case fails writing its summary to stdout", body: `[{"name":"alpha","archived":false},{"name":"BETA","archived":false},{"name":"gamma","archived":false}]`, healthyStatus: 0, failStdout: true},
		{name: "gaps case fails writing a gap to stderr", body: `[{"name":"alpha","archived":false},{"name":"BETA","archived":false},{"name":"gamma","archived":false},{"name":"stray","archived":false}]`, healthyStatus: 1, failStdout: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				if _, err := io.WriteString(writer, test.body); err != nil {
					t.Errorf("write response: %v", err)
				}
			}))
			t.Cleanup(server.Close)
			options := func(stdout, stderr io.Writer) repoInventoryOptions {
				return repoInventoryOptions{
					reposText: testManagedRepos,
					mint: func(context.Context) (githubapptoken.Token, error) {
						return "test-token", nil
					},
					client: &http.Client{Transport: testRoundTripper{base: server.Client().Transport, url: server.URL}},
					stdout: stdout,
					stderr: stderr,
				}
			}
			var stdout, stderr strings.Builder
			if code := runRepoInventoryWithOptions(t.Context(), options(&stdout, &stderr)); code != test.healthyStatus {
				t.Fatalf("runRepoInventoryWithOptions() = %d, stdout %q, stderr %q; want %d", code, stdout.String(), stderr.String(), test.healthyStatus)
			}
			var failingStdout, failingStderr io.Writer = io.Discard, io.Discard
			if test.failStdout {
				failingStdout = errorWriter{}
			} else {
				failingStderr = errorWriter{}
			}
			if code := runRepoInventoryWithOptions(t.Context(), options(failingStdout, failingStderr)); code != 2 {
				t.Fatalf("runRepoInventoryWithOptions() with failing writer = %d, want 2", code)
			}
		})
	}
}

type testRoundTripper struct {
	base http.RoundTripper
	url  string
}

func (transport testRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = strings.TrimPrefix(transport.url, "http://")
	return transport.base.RoundTrip(clone)
}
