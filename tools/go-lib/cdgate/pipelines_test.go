package cdgate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RigelBuild/nebula/tools/go-lib/woodpecker"
)

// wpServer serves canned Woodpecker responses keyed by request path and query.
// Unknown requests fail the test, so a stray call cannot pass silently.
func wpServer(t *testing.T, routes map[string]string) (*woodpecker.Client, func() []string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Path
		if r.URL.RawQuery != "" {
			key += "?" + r.URL.RawQuery
		}
		seen = append(seen, key)
		body, ok := routes[key]
		if !ok {
			t.Errorf("unexpected request %q", key)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return &woodpecker.Client{BaseURL: srv.URL, Token: "t", HTTP: srv.Client()}, func() []string { return seen }
}

func pipelineJSON(t *testing.T, number int64, commit, branch, event string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"number": number, "commit": commit, "branch": branch, "event": event})
	if err != nil {
		t.Fatalf("marshal pipeline test fixture: %v", err)
	}
	return string(b)
}

// TestFetchPipelineWithoutWorkflowsIsEmpty: a detail with no workflows key
// reads as no workflows, not an error, as the TS reader does.
func TestFetchPipelineWithoutWorkflowsIsEmpty(t *testing.T) {
	client, _ := wpServer(t, map[string]string{
		"/api/repos/7/pipelines/12": `{"number":12,"commit":"` + shaA + `","branch":"main","event":"push"}`,
	})
	got, err := PipelineAPI{Client: client, RepoID: 7}.Fetch(t.Context(), 12)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got.Workflows) != 0 {
		t.Fatalf("workflows = %v, want none", got.Workflows)
	}
}

func TestFetchPipelineReadsWorkflowStates(t *testing.T) {
	client, _ := wpServer(t, map[string]string{
		"/api/repos/7/pipelines/12": `{"number":12,"commit":"` + shaA + `","branch":"main","event":"push",` +
			`"workflows":[{"name":"deploy","state":"success"},{"name":"test","state":"running"}]}`,
	})
	p := PipelineAPI{Client: client, RepoID: 7}
	got, err := p.Fetch(context.Background(), 12)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.Number != 12 || got.Commit != shaA {
		t.Fatalf("got %+v, want number 12 at %s", got, shaA)
	}
	if got.Workflows["deploy"] != "success" || got.Workflows["test"] != "running" {
		t.Fatalf("workflows = %v", got.Workflows)
	}
}

func TestFetchPipelineRejectsInvalidWorkflow(t *testing.T) {
	client, _ := wpServer(t, map[string]string{
		"/api/repos/7/pipelines/12": `{"number":12,"commit":"` + shaA + `","branch":"main","event":"push","workflows":[{"name":"deploy"}]}`,
	})
	_, err := (PipelineAPI{Client: client, RepoID: 7}).Fetch(context.Background(), 12)
	if err == nil || !strings.Contains(err.Error(), "invalid workflow") {
		t.Fatalf("Fetch err = %v; want the invalid-workflow error for a workflow without a state", err)
	}
}

// TestFetchPipelineRejectsNumberMismatch matches the TS fetchPipeline: a
// response for another pipeline number is an error, not a silent read.
func TestFetchPipelineRejectsNumberMismatch(t *testing.T) {
	client, _ := wpServer(t, map[string]string{
		"/api/repos/7/pipelines/12": `{"number":13,"commit":"` + shaA + `","branch":"main","event":"push","workflows":[{"name":"deploy","state":"success"}]}`,
	})
	_, err := (PipelineAPI{Client: client, RepoID: 7}).Fetch(context.Background(), 12)
	if err == nil || err.Error() != "pipeline number mismatch" {
		t.Fatalf("Fetch err = %v; want \"pipeline number mismatch\" for a response naming 13", err)
	}
}

func TestMainPipelinesRejectsItemWithoutCommit(t *testing.T) {
	client, _ := wpServer(t, map[string]string{
		"/api/repos/7/pipelines?branch=main&event=push%2Cmanual&page=1&perPage=50": `[{"number":9,"commit":"","branch":"main","event":"push"}]`,
	})
	_, err := (PipelineAPI{Client: client, RepoID: 7}).MainPipelines(context.Background(), 1)
	if err == nil || err.Error() != "invalid pipeline list item fields" {
		t.Fatalf("MainPipelines err = %v; want the invalid-fields error for an empty commit", err)
	}
}

// TestMainPipelinesRejectsNonPositiveNumber matches the TS list-item rule
// number >= 1: pipeline 0 is not a pipeline.
func TestMainPipelinesRejectsNonPositiveNumber(t *testing.T) {
	client, _ := wpServer(t, map[string]string{
		"/api/repos/7/pipelines?branch=main&event=push%2Cmanual&page=1&perPage=50": `[{"number":0,"commit":"` + shaA + `","branch":"main","event":"push"}]`,
	})
	if _, err := (PipelineAPI{Client: client, RepoID: 7}).MainPipelines(context.Background(), 1); err == nil {
		t.Fatal("MainPipelines accepted pipeline number 0; want the invalid-fields error")
	}
}

// TestMainPipelinesRejectsNonStringBranch matches the TS list-item rule that
// branch must be a string.
func TestMainPipelinesRejectsNonStringBranch(t *testing.T) {
	client, _ := wpServer(t, map[string]string{
		"/api/repos/7/pipelines?branch=main&event=push%2Cmanual&page=1&perPage=50": `[{"number":9,"commit":"` + shaA + `","branch":42,"event":"push"}]`,
	})
	if _, err := (PipelineAPI{Client: client, RepoID: 7}).MainPipelines(context.Background(), 1); err == nil {
		t.Fatal("MainPipelines accepted a non-string branch; want the invalid-fields error")
	}
}

func TestMainPipelinesStopsAtOlderNumberAndSkipsOwn(t *testing.T) {
	// Newest first: 30 is newer, 20 is the own pipeline (skipped), 19 is older
	// than own and ends the scan. 18 must never be read.
	page := "[" + strings.Join([]string{
		pipelineJSON(t, 30, shaA, "main", "push"),
		pipelineJSON(t, 20, shaB, "main", "push"),
		pipelineJSON(t, 19, shaC, "main", "push"),
		pipelineJSON(t, 18, shaA, "main", "push"),
	}, ",") + "]"
	client, seen := wpServer(t, map[string]string{
		"/api/repos/7/pipelines?branch=main&event=push%2Cmanual&page=1&perPage=50": page,
	})
	items, err := PipelineAPI{Client: client, RepoID: 7}.MainPipelines(context.Background(), 20)
	if err != nil {
		t.Fatalf("MainPipelines: %v", err)
	}
	if len(items) != 1 || items[0].Number != 30 {
		t.Fatalf("items = %+v, want only the newer pipeline 30", items)
	}
	if len(seen()) != 1 {
		t.Fatalf("requests = %v, want one page: an older pipeline ends the scan", seen())
	}
}

func TestMainPipelinesFiltersBranchAndEvent(t *testing.T) {
	page := "[" + strings.Join([]string{
		pipelineJSON(t, 30, shaA, "feature", "push"),
		pipelineJSON(t, 29, shaB, "main", "pull_request"),
		pipelineJSON(t, 28, shaC, "main", "manual"),
	}, ",") + "]"
	client, _ := wpServer(t, map[string]string{
		"/api/repos/7/pipelines?branch=main&event=push%2Cmanual&page=1&perPage=50": page,
	})
	items, err := PipelineAPI{Client: client, RepoID: 7}.MainPipelines(context.Background(), 1)
	if err != nil {
		t.Fatalf("MainPipelines: %v", err)
	}
	if len(items) != 1 || items[0].Number != 28 {
		t.Fatalf("items = %+v, want only the main manual pipeline 28", items)
	}
}

func TestMainPipelinesFollowsFullPages(t *testing.T) {
	// A full page of 50 means more history may follow; the next page is read.
	full := make([]string, 0, pageSize)
	for i := range pageSize {
		full = append(full, pipelineJSON(t, int64(200-i), shaA, "main", "push"))
	}
	pageOne := "[" + strings.Join(full, ",") + "]"
	pageTwo := "[" + pipelineJSON(t, 1, shaB, "main", "push") + "]"
	client, seen := wpServer(t, map[string]string{
		"/api/repos/7/pipelines?branch=main&event=push%2Cmanual&page=1&perPage=50": pageOne,
		"/api/repos/7/pipelines?branch=main&event=push%2Cmanual&page=2&perPage=50": pageTwo,
	})
	items, err := PipelineAPI{Client: client, RepoID: 7}.MainPipelines(context.Background(), 0)
	if err != nil {
		t.Fatalf("MainPipelines: %v", err)
	}
	if len(seen()) != 2 {
		t.Fatalf("requests = %v, want the full page followed by page 2", seen())
	}
	if len(items) != pageSize+1 {
		t.Fatalf("len(items) = %d, want %d", len(items), pageSize+1)
	}
}

func TestMainPipelinesRejectsNonListResponse(t *testing.T) {
	client, _ := wpServer(t, map[string]string{
		"/api/repos/7/pipelines?branch=main&event=push%2Cmanual&page=1&perPage=50": `{"not":"a list"}`,
	})
	if _, err := (PipelineAPI{Client: client, RepoID: 7}).MainPipelines(context.Background(), 1); err == nil || err.Error() != "invalid pipeline list" {
		t.Fatalf("err = %v, want the invalid-list error", err)
	}
}

// statusServer answers every request with status and no body, so a read fails
// with a non-2xx StatusError, the shape a rejected or unavailable API returns.
func statusServer(t *testing.T, status int) *woodpecker.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return &woodpecker.Client{BaseURL: srv.URL, Token: "tok", HTTP: srv.Client()}
}

// TestPipelineListRejectedCredentialBlocks: a 401 or 403 from the first
// pipeline-list read is a rejected token, so the read returns an invalid API
// context and the gate blocks.
func TestPipelineListRejectedCredentialBlocks(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		api := PipelineAPI{Client: statusServer(t, status), RepoID: 7}
		_, err := api.MainPipelines(context.Background(), 1)
		if !errors.Is(err, woodpecker.ErrInvalidAPIContext) {
			t.Fatalf("status %d: err = %v, want ErrInvalidAPIContext so the gate blocks", status, err)
		}
	}
}

// TestPipelineFetchRejectedCredentialBlocks: the same rule holds for the own
// pipeline read, which runs before the list.
func TestPipelineFetchRejectedCredentialBlocks(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		api := PipelineAPI{Client: statusServer(t, status), RepoID: 7}
		_, err := api.Fetch(context.Background(), 12)
		if !errors.Is(err, woodpecker.ErrInvalidAPIContext) {
			t.Fatalf("status %d: err = %v, want ErrInvalidAPIContext so the gate blocks", status, err)
		}
	}
}

// TestPipelineListServerErrorFailsOpen: a 5xx is transient, so it must not be
// classified as an invalid context. The gate then fails open and deploys.
func TestPipelineListServerErrorFailsOpen(t *testing.T) {
	api := PipelineAPI{Client: statusServer(t, http.StatusInternalServerError), RepoID: 7}
	_, err := api.MainPipelines(context.Background(), 1)
	if err == nil {
		t.Fatal("a 500 from the pipeline list returned no error")
	}
	if errors.Is(err, woodpecker.ErrInvalidAPIContext) {
		t.Fatalf("a transient 500 was classified as an invalid API context: %v", err)
	}
}
