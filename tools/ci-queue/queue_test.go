package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseQueueTaskAllowsAbsentLabels(t *testing.T) {
	value := map[string]any{
		"id": "task", "name": "build", "created": float64(1),
		"pipeline_number": float64(7), "agent_id": float64(0), "labels": nil,
	}
	task, err := parseQueueTask(value)
	if err != nil {
		t.Fatal(err)
	}
	if len(task.Labels) != 0 {
		t.Fatalf("labels = %#v, want no labels", task.Labels)
	}
}

func TestRowsForPipelines(t *testing.T) {
	tests := []struct {
		name      string
		info      QueueInfo
		pipelines []float64
		now       float64
		repo      string
		want      []QueueRow
	}{
		{
			name: "joins selected repository only",
			info: QueueInfo{Pending: []QueueTask{
				testTask(),
				testTaskWith("other", "build", "Other/repo", 900),
				testTaskWith("missing", "build", "", 900),
			}},
			pipelines: []float64{7},
			now:       1000,
			repo:      "example-owner/example-repo",
			want: []QueueRow{{
				Workflow: "build", State: QueuePending, AgeSeconds: 100,
				Pipeline: 7, Rank: new(1),
			}},
		},
		{
			name:      "future creation time has zero age",
			info:      QueueInfo{Pending: []QueueTask{testTaskWith("future", "build", "example-owner/example-repo", 1100)}},
			pipelines: []float64{7},
			now:       1000,
			repo:      "example-owner/example-repo",
			want: []QueueRow{{
				Workflow: "build", State: QueuePending, AgeSeconds: 0,
				Pipeline: 7, Rank: new(1),
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rowsForPipelines(tt.info, tt.pipelines, tt.now, tt.repo)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("rowsForPipelines() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestRankPending(t *testing.T) {
	first := testTaskWith("b", "z", "example-owner/example-repo", 900)
	second := testTaskWith("a", "a", "example-owner/example-repo", 900)
	queue := QueueInfo{Pending: []QueueTask{first, second}}
	if got := rankPending(queue, second); got == nil || *got != 1 {
		t.Fatalf("rankPending(second) = %v, want 1", got)
	}
	if got := rankPending(queue, first); got == nil || *got != 2 {
		t.Fatalf("rankPending(first) = %v, want 2", got)
	}
}

func TestRankPendingUsesEnglishLocaleForTiedNames(t *testing.T) {
	uppercase := testTaskWith("uppercase", "Zeta", "example-owner/example-repo", 900)
	lowercase := testTaskWith("lowercase", "alpha", "example-owner/example-repo", 900)
	queue := QueueInfo{Pending: []QueueTask{uppercase, lowercase}}
	if got := rankPending(queue, lowercase); got == nil || *got != 1 {
		t.Fatalf("rankPending(lowercase) = %v, want 1", got)
	}
	if got := rankPending(queue, uppercase); got == nil || *got != 2 {
		t.Fatalf("rankPending(uppercase) = %v, want 2", got)
	}
}

func TestRowsForPipelinesRunningRowsExposeAgent(t *testing.T) {
	task := testTaskWith("id", "build", "example-owner/example-repo", 900)
	task.AgentName = "runner-1"
	queue := QueueInfo{Running: []QueueTask{task}}
	got := rowsForPipelines(queue, []float64{7}, 1000, "example-owner/example-repo")
	if len(got) != 1 || got[0].State != QueueRunning || got[0].Rank != nil || got[0].AgentName == nil || *got[0].AgentName != "runner-1" {
		t.Fatalf("running row = %#v", got)
	}
}

func TestRenderTableEscapesControls(t *testing.T) {
	got := renderTable([]QueueRow{{Workflow: "a\tb\n", State: QueuePending, AgeSeconds: 1, Pipeline: 7, Rank: new(1)}}, "branch")
	if !strings.Contains(got, "a\\tb\\n") {
		t.Fatalf("renderTable() = %q, want escaped control characters", got)
	}
}

func TestParseArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    Options
		wantErr string
	}{
		{name: "positive pipeline", args: []string{"--pipeline", "7"}, want: Options{Pipeline: 7, HasPipeline: true}},
		{name: "invalid zero pipeline", args: []string{"--pipeline", "0"}, wantErr: "ci-queue: --pipeline needs a positive integer"},
		{name: "missing branch value", args: []string{"--branch"}, wantErr: "ci-queue: --branch needs a value"},
		{name: "unknown argument", args: []string{"--nope"}, wantErr: "ci-queue: unknown argument"},
		{name: "help", args: []string{"--help"}, want: Options{Help: true}},
		{name: "pipeline must be a positive integer", args: []string{"--pipeline", "1.5"}, wantErr: "ci-queue: --pipeline needs a positive integer"},
		{name: "short help", args: []string{"-h"}, want: Options{Help: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseArgs(tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseArgs() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseArgs() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestReadWoodpeckerTokenPrefersFileOnEachRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	getenv := testEnvironment(map[string]string{"WOODPECKER_TOKEN_FILE": path, "WOODPECKER_TOKEN": "env-token"})
	for _, token := range []string{"first-token", "rotated-token"} {
		if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := readWoodpeckerToken(getenv); got != token {
			t.Fatalf("readWoodpeckerToken() = %q, want %q", got, token)
		}
	}
}

func TestPipelineNumbersForBranchBoundsPaging(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		pipelines := make([]map[string]any, pipelinesPageSize)
		for index := range pipelines {
			pipelines[index] = map[string]any{"number": index + 1, "branch": "main"}
		}
		writeJSON(t, writer, http.StatusOK, pipelines)
	}))
	defer server.Close()

	client := newAPI(server.URL, "token", server.Client())
	if _, err := pipelineNumbersForBranch(t.Context(), client, 2, "main"); err != nil {
		t.Fatal(err)
	}
	if calls != pipelinesMaxPages {
		t.Fatalf("pipeline requests = %d, want %d", calls, pipelinesMaxPages)
	}
}

func TestPipelineNumbersForBranchNetworkFailureHasDeadline(t *testing.T) {
	client := newAPI("https://wp", "token", &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if _, ok := request.Context().Deadline(); !ok {
			t.Error("request context has no timeout deadline")
		}
		return nil, errors.New("network down")
	})})
	_, err := pipelineNumbersForBranch(t.Context(), client, 2, "main")
	if err == nil || !strings.Contains(err.Error(), "network down") {
		t.Fatalf("pipelineNumbersForBranch() error = %v, want network failure", err)
	}
}

func TestRunHelpWorksWithoutCredentials(t *testing.T) {
	var output strings.Builder
	err := runWithDependencies(t.Context(), []string{"--help"}, dependencies{Getenv: emptyEnvironment, Stdout: &output})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "usage: ci-queue") {
		t.Fatalf("help output = %q", output.String())
	}
}

func TestRunRequiresRepoWhenNoFlagOrEnvironment(t *testing.T) {
	err := runWithDependencies(t.Context(), []string{"--branch", "main"}, dependencies{
		Getenv: testEnvironment(map[string]string{"WOODPECKER_SERVER": "https://ci.example.test", "WOODPECKER_TOKEN": "token"}),
		Stdout: io.Discard,
	})
	if err == nil || err.Error() != "ci-queue: --repo or WOODPECKER_REPO is required" {
		t.Fatalf("err = %v, want the missing repo error", err)
	}
}

func TestRunUsesBranchEnvironmentBeforeGit(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.RequestURI())
		switch {
		case strings.Contains(request.URL.Path, "/lookup/"):
			writeJSON(t, writer, http.StatusOK, map[string]any{"id": 2})
		case strings.HasSuffix(request.URL.Path, "/pipelines"):
			writeJSON(t, writer, http.StatusOK, []map[string]any{})
		default:
			writeJSON(t, writer, http.StatusOK, map[string]any{"pending": []any{}, "waiting_on_deps": []any{}, "running": []any{}})
		}
	}))
	defer server.Close()

	err := runWithDependencies(t.Context(), nil, dependencies{
		Getenv: testEnvironment(map[string]string{"WOODPECKER_SERVER": server.URL, "WOODPECKER_TOKEN": "token", "WOODPECKER_REPO": "example-owner/example-repo", "WOODPECKER_BRANCH": "feature/name with spaces"}),
		Client: server.Client(),
		ResolveBranch: func(context.Context) (string, error) {
			t.Fatal("git branch resolver called despite WOODPECKER_BRANCH")
			return "", nil
		},
		Stdout: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 3 || paths[1] != "/api/repos/2/pipelines?branch=feature%2Fname%20with%20spaces&page=1&perPage=50" {
		t.Fatalf("request paths = %v, want encoded branch pipeline request", paths)
	}
}

func TestRunOrchestratesRepoLookupPipelineListAndQueueFetch(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.EscapedPath())
		switch {
		case strings.Contains(request.URL.Path, "/lookup/"):
			writeJSON(t, writer, http.StatusOK, map[string]any{"id": 2})
		case strings.HasSuffix(request.URL.Path, "/pipelines"):
			writeJSON(t, writer, http.StatusOK, []map[string]any{{"number": 7, "branch": "main", "status": "pending", "created": 1}})
		default:
			writeJSON(t, writer, http.StatusOK, map[string]any{
				"pending": []map[string]any{{
					"id": "task", "name": "build", "created": 1, "pipeline_number": 7, "agent_id": 0,
					"labels": map[string]string{"repo": "example-owner/example-repo"},
				}},
				"waiting_on_deps": []any{}, "running": []any{},
			})
		}
	}))
	defer server.Close()

	var output strings.Builder
	err := runWithDependencies(t.Context(), []string{"--branch", "main"}, dependencies{
		Getenv: testEnvironment(map[string]string{"WOODPECKER_SERVER": server.URL, "WOODPECKER_TOKEN": "token", "WOODPECKER_REPO": "example-owner/example-repo"}),
		Client: server.Client(),
		Now:    func() time.Time { return time.Unix(10, 0) },
		Stdout: &output,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantPaths := []string{"/api/repos/lookup/example-owner%2Fexample-repo", "/api/repos/2/pipelines", "/api/queue/info"}
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Fatalf("request paths = %v, want %v", paths, wantPaths)
	}
	if !strings.Contains(output.String(), "build\tpending") {
		t.Fatalf("queue output = %q", output.String())
	}
}

func TestRunReadsTokenFileOnEachCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	var authorizations []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		authorizations = append(authorizations, request.Header.Get("Authorization"))
		switch {
		case strings.Contains(request.URL.Path, "/lookup/"):
			writeJSON(t, writer, http.StatusOK, map[string]any{"id": 2})
		case strings.HasSuffix(request.URL.Path, "/pipelines"):
			writeJSON(t, writer, http.StatusOK, []map[string]any{{"number": 7, "branch": "main"}})
		default:
			writeJSON(t, writer, http.StatusOK, map[string]any{"pending": []any{}, "waiting_on_deps": []any{}, "running": []any{}})
		}
	}))
	defer server.Close()

	values := map[string]string{"WOODPECKER_SERVER": server.URL, "WOODPECKER_TOKEN_FILE": path, "WOODPECKER_TOKEN": "env-token", "WOODPECKER_REPO": "example-owner/example-repo"}
	deps := dependencies{Getenv: testEnvironment(values), Client: server.Client(), Stdout: &strings.Builder{}}
	for _, token := range []string{"file-token", "rotated-token"} {
		if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := runWithDependencies(t.Context(), []string{"--branch", "main"}, deps); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"Bearer file-token", "Bearer file-token", "Bearer file-token", "Bearer rotated-token", "Bearer rotated-token", "Bearer rotated-token"}
	if !reflect.DeepEqual(authorizations, want) {
		t.Fatalf("authorization headers = %v, want %v", authorizations, want)
	}
}

func TestRunExplicitPipelineIsVerifiedBeforeQueueJoin(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.EscapedPath())
		switch request.URL.EscapedPath() {
		case "/api/repos/lookup/example-owner%2Fexample-repo":
			writeJSON(t, writer, http.StatusOK, map[string]any{"id": 2})
		case "/api/repos/2/pipelines/7":
			writeJSON(t, writer, http.StatusOK, map[string]any{"number": 7, "branch": "main"})
		case "/api/queue/info":
			writeJSON(t, writer, http.StatusOK, map[string]any{
				"pending": []map[string]any{
					{"id": "selected", "name": "selected", "created": 900, "pipeline_number": 7, "agent_id": 0, "labels": map[string]string{"repo": "example-owner/example-repo"}},
					{"id": "other", "name": "other-repository", "created": 900, "pipeline_number": 7, "agent_id": 0, "labels": map[string]string{"repo": "Other/repo"}},
				},
				"waiting_on_deps": []any{}, "running": []any{},
			})
		default:
			t.Errorf("unexpected request %s", request.URL.Path)
			writeJSON(t, writer, http.StatusNotFound, map[string]string{"error": "not found"})
		}
	}))
	defer server.Close()

	var output strings.Builder
	err := runWithDependencies(t.Context(), []string{"--repo", "example-owner/example-repo", "--pipeline", "7"}, dependencies{
		Getenv: testEnvironment(map[string]string{"WOODPECKER_SERVER": server.URL, "WOODPECKER_TOKEN": "token"}),
		Client: server.Client(),
		Now:    func() time.Time { return time.Unix(1000, 0) },
		Stdout: &output,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantPaths := []string{"/api/repos/lookup/example-owner%2Fexample-repo", "/api/repos/2/pipelines/7", "/api/queue/info"}
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Fatalf("request paths = %v, want %v", paths, wantPaths)
	}
	if !strings.Contains(output.String(), "selected\tpending") || strings.Contains(output.String(), "other-repository") {
		t.Fatalf("queue output did not filter selected repository: %q", output.String())
	}
}

func TestParsePipelinesRejectsInvalidNumber(t *testing.T) {
	if _, err := parsePipelines([]any{map[string]any{"number": float64(0), "branch": "main"}}); err == nil {
		t.Fatal("parsePipelines() accepted a non-positive pipeline number")
	}
}

func TestRenderTableEmptyOutput(t *testing.T) {
	if got := renderTable(nil, "branch"); got != "no queued tasks for branch\n" {
		t.Fatalf("renderTable() = %q", got)
	}
}

func TestReadWoodpeckerToken(t *testing.T) {
	tests := []struct {
		name  string
		file  string
		token string
		want  string
	}{
		{name: "non-empty file wins and trims newline", file: "file-token\n", token: "env-token", want: "file-token"},
		{name: "file content whitespace trimmed", file: "  token  \n", want: "token"},
		{name: "empty file falls back to environment", file: " \n\t ", token: "env-token", want: "env-token"},
		{name: "missing file falls back to environment", token: "env-token", want: "env-token"},
		{name: "directory falls back to environment", token: "env-token", want: "env-token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := t.TempDir()
			if tt.name == "directory falls back to environment" {
				got := readWoodpeckerToken(testEnvironment(map[string]string{"WOODPECKER_TOKEN_FILE": path, "WOODPECKER_TOKEN": tt.token}))
				if got != tt.want {
					t.Fatalf("readWoodpeckerToken() = %q, want %q", got, tt.want)
				}
				return
			}
			if tt.file != "" {
				path = filepath.Join(path, "token")
				if err := os.WriteFile(path, []byte(tt.file), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				path = filepath.Join(path, "missing")
			}
			got := readWoodpeckerToken(testEnvironment(map[string]string{"WOODPECKER_TOKEN_FILE": path, "WOODPECKER_TOKEN": tt.token}))
			if got != tt.want {
				t.Fatalf("readWoodpeckerToken() = %q, want %q", got, tt.want)
			}
		})
	}
}

func testTask() QueueTask {
	return testTaskWith("id", "build", "example-owner/example-repo", 900)
}

func testTaskWith(id, name, repo string, created float64) QueueTask {
	var labels map[string]string
	if repo != "" {
		labels = map[string]string{"repo": repo}
	}
	return QueueTask{ID: id, Name: name, Labels: labels, AgentID: 0, Created: created, PipelineNumber: 7}
}

func testEnvironment(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

func emptyEnvironment(string) (string, bool) { return "", false }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func writeJSON(t *testing.T, writer http.ResponseWriter, status int, body any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(body); err != nil {
		t.Errorf("write JSON test response: %v", err)
	}
}
