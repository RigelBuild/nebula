package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestStatusAcrossRepositories(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	for _, e := range []LedgerEntry{
		{Repo: "example/app", PR: 1, Bookmark: "test-lane/ex-1-example", Codename: "test-lane"},
		{Repo: "example/docs", PR: 2, Bookmark: "test-lane/ex-2-example", Codename: "test-lane"},
		{Repo: "example/docs", PR: 3, Bookmark: "other/ex-3-example", Codename: "other"},
	} {
		if err := (Ledger{root: f.repo.stateDir}).Save(e); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var request struct {
			Query     string            `json:"query"`
			Variables map[string]string `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if r.URL.Path != "/graphql" || strings.Contains(request.Query, "number:3") {
			t.Errorf("unexpected query: %s", request.Query)
		}
		checks := []any{map[string]any{"__typename": "CheckRun", "name": "build", "status": "COMPLETED", "conclusion": "SUCCESS"}, map[string]any{"__typename": "StatusContext", "context": "deploy", "state": "FAILURE"}}
		contexts := map[string]any{"nodes": checks}
		commit := map[string]any{"statusCheckRollup": map[string]any{"contexts": contexts}}
		pr := map[string]any{"state": "OPEN", "isDraft": true, "mergeable": "MERGEABLE", "headRefOid": strings.Repeat("a", 40), "baseRefName": "main", "comments": map[string]any{"nodes": []any{}}, "timelineItems": map[string]any{"nodes": []any{}}, "commits": map[string]any{"nodes": []any{map[string]any{"commit": commit}}}}
		respondJSON(t, w, map[string]any{"data": map[string]any{"viewer": map[string]string{"login": "example-agent"}, "repository": map[string]any{"p0": pr}}})
	}))
	defer server.Close()
	s, _ := service(f.ctx)
	s.forge = &forgeClient{client: server.Client(), baseURL: server.URL}
	rows, err := f.repo.status(f.ctx, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || calls != 2 {
		t.Fatalf("rows=%+v calls=%d", rows, calls)
	}
	for _, row := range rows {
		if row.CI != "red: deploy" || row.State != "draft" {
			t.Fatalf("row=%+v", row)
		}
	}
	encoded, err := json.Marshal(rows)
	if err != nil || strings.Contains(string(encoded), "thread") {
		t.Fatalf("json=%s err=%v", encoded, err)
	}
}

func TestStatusRejectsPaginatedComments(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	entry := LedgerEntry{Repo: "example/app", PR: 1, Bookmark: "test-lane/ex-1-example", Codename: "test-lane"}
	if err := (Ledger{root: f.repo.stateDir}).Save(entry); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respondJSON(t, w, map[string]any{"data": map[string]any{"repository": map[string]any{"p0": map[string]any{
			"state": "OPEN", "comments": map[string]any{"pageInfo": map[string]bool{"hasNextPage": true}},
		}}}})
	}))
	defer server.Close()
	s, _ := service(f.ctx)
	s.forge = &forgeClient{client: server.Client(), baseURL: server.URL}
	_, err := f.repo.status(f.ctx, statusOptions{})
	if err == nil || !strings.Contains(err.Error(), entry.Repo) {
		t.Fatalf("expected pagination error naming %s, got %v", entry.Repo, err)
	}
}

func TestStatusQueriesLedgerInChunks(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	count := 60 // three 25-entry chunks
	wantPRs := make([]int, 0, count)
	for i := range count {
		pr := 100 + i
		wantPRs = append(wantPRs, pr)
		entry := LedgerEntry{Repo: "example/app", PR: pr, Bookmark: fmt.Sprintf("test-lane/ex-%d-example", pr), Codename: "test-lane"}
		if err := (Ledger{root: f.repo.stateDir}).Save(entry); err != nil {
			t.Fatal(err)
		}
	}
	// Three-digit PR numbers sort the same lexically (ledger walk order) and numerically.
	aliasRE := regexp.MustCompile(`p(\d+):pullRequest\(number:(\d+)\)`)
	var mu sync.Mutex
	queries := 0
	queried := []int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		queries++
		repository := map[string]any{}
		for _, m := range aliasRE.FindAllStringSubmatch(request.Query, -1) {
			number, err := strconv.Atoi(m[2])
			if err != nil {
				t.Error(err)
				return
			}
			queried = append(queried, number)
			checks := []any{map[string]any{"__typename": "CheckRun", "name": "build-" + m[2], "status": "COMPLETED", "conclusion": "FAILURE"}}
			commit := map[string]any{"statusCheckRollup": map[string]any{"contexts": map[string]any{"nodes": checks}}}
			repository["p"+m[1]] = map[string]any{"state": "OPEN", "isDraft": true, "mergeable": "MERGEABLE", "headRefOid": strings.Repeat("a", 40), "baseRefName": "main", "comments": map[string]any{"nodes": []any{}}, "timelineItems": map[string]any{"nodes": []any{}}, "commits": map[string]any{"nodes": []any{map[string]any{"commit": commit}}}}
		}
		respondJSON(t, w, map[string]any{"data": map[string]any{"viewer": map[string]string{"login": "example-agent"}, "repository": repository}})
	}))
	defer server.Close()
	s, _ := service(f.ctx)
	s.forge = &forgeClient{client: server.Client(), baseURL: server.URL}
	rows, err := f.repo.status(f.ctx, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if queries != 3 {
		t.Fatalf("queries=%d want 3", queries)
	}
	slices.Sort(queried)
	if !slices.Equal(queried, wantPRs) {
		t.Fatalf("queried=%v want %v", queried, wantPRs)
	}
	if len(rows) != count {
		t.Fatalf("rows=%d want %d", len(rows), count)
	}
	for i, row := range rows {
		if row.PR != wantPRs[i] || row.CI != fmt.Sprintf("red: build-%d", wantPRs[i]) {
			t.Fatalf("row %d=%+v want PR %d in ledger order", i, row, wantPRs[i])
		}
	}
}
