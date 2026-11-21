package linear

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchPageWithPaginationAndProjection(t *testing.T) {
	fullNode := issueNode{
		Identifier:  "SEA-1",
		Title:       "t",
		URL:         "u",
		Description: new("d"),
		State:       &issueState{Name: "In Progress", Type: "started"},
		Parent:      &issueParent{Identifier: "SEA-0"},
		CompletedAt: new("2026-08-20T10:00:00.000Z"),
	}
	tests := []pageCase{
		{name: "first page sends null cursor and returns nested fields", response: pageResponse(true, "cursor-1", fullNode), wantCursor: "cursor-1", wantIssues: 1},
		{name: "later page forwards cursor", response: pageResponse(false, "cursor-9"), after: "cursor-2", wantCursor: "cursor-9", wantAfter: "cursor-2"},
		{name: "missing cursor fails instead of truncating", response: pageResponse(true, ""), wantErr: "truncated"},
		{name: "last page returns cursor", response: pageResponse(false, "cursor-9"), wantCursor: "cursor-9"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) { runPageCase(t, test) })
	}
	response := pageResponse(false, "")
	response.Issues.Nodes = []issueNode{{Identifier: "SEA-2", Title: "t2", URL: "u2"}}
	page, err := fetchPageWith(t.Context(), func(context.Context, string, map[string]any) (pageResponseData, error) {
		return response, nil
	}, "")
	if err != nil || len(page.Issues) != 1 || page.Issues[0].StateType != "" || page.Issues[0].ParentIdentifier != "" || page.Issues[0].CompletedAt != nil {
		t.Fatalf("nullable node projection = %#v, %v", page, err)
	}
}

type pageCase struct {
	name                       string
	response                   pageResponseData
	after, wantCursor, wantErr string
	wantAfter                  any
	wantIssues                 int
}

func runPageCase(t *testing.T, test pageCase) {
	t.Helper()
	calls := 0
	request := func(_ context.Context, query string, variables map[string]any) (pageResponseData, error) {
		calls++
		if calls != 1 {
			t.Fatalf("request called %d times", calls)
		}
		query = strings.Join(strings.Fields(query), " ")
		if !strings.Contains(query, "state { name type }") || !strings.Contains(query, "parent { identifier }") || !strings.Contains(query, "completedAt") {
			t.Fatalf("page query missing fields: %s", query)
		}
		if variables["teamKey"] != "RIG" || variables["first"] != 100 || variables["after"] != test.wantAfter {
			t.Fatalf("variables = %#v", variables)
		}
		return test.response, nil
	}
	got, err := fetchPageWith(t.Context(), request, test.after)
	if test.wantErr != "" {
		if err == nil || !strings.Contains(err.Error(), test.wantErr) {
			t.Fatalf("error = %v, want %q", err, test.wantErr)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if got.EndCursor != test.wantCursor || len(got.Issues) != test.wantIssues {
		t.Fatalf("page = %#v", got)
	}
	if test.wantIssues == 1 {
		issue := got.Issues[0]
		if issue.Identifier != "SEA-1" || issue.StateName != "In Progress" || issue.StateType != "started" || issue.ParentIdentifier != "SEA-0" || issue.CompletedAt == nil || *issue.CompletedAt != "2026-08-20T10:00:00.000Z" {
			t.Fatalf("projected issue = %#v", issue)
		}
	}
}

func pageResponse(hasNext bool, cursor string, nodes ...issueNode) pageResponseData {
	var response pageResponseData
	response.Issues.Nodes = nodes
	response.Issues.PageInfo.HasNextPage = hasNext
	if cursor != "" {
		response.Issues.PageInfo.EndCursor = new(cursor)
	}
	return response
}

func TestIssueClientPageFetchAndUpdate(t *testing.T) {
	var calls []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handlePageRequest(t, &calls, w, r) }))
	defer server.Close()
	api, err := NewIssueAPI(t.Context(), server.Client(), server.URL, func(name string) string {
		if name == "LINEAR_API_KEY" {
			return "api-key"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := api.FetchPage(t.Context(), "cursor-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Issues) != 2 || page.EndCursor != "c9" {
		t.Fatalf("page = %#v", page)
	}
	if page.Issues[0].StateName != "In Progress" || page.Issues[0].ParentIdentifier != "SEA-0" || page.Issues[1].StateType != "" {
		t.Fatalf("page projection = %#v", page.Issues)
	}
	if err := api.UpdateDescription(t.Context(), "SEA-1", "new body"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("HTTP calls = %#v", calls)
	}
	pageCall := calls[0]
	query := strings.Join(strings.Fields(pageCall["query"].(string)), " ")
	if !strings.Contains(query, "state { name type }") || !strings.Contains(query, "parent { identifier }") || !strings.Contains(query, "completedAt") {
		t.Fatalf("query missing required fields: %s", query)
	}
	vars := pageCall["variables"].(map[string]any)
	if vars["teamKey"] != "RIG" || vars["first"] != float64(100) || vars["after"] != "cursor-2" {
		t.Fatalf("page variables = %#v", vars)
	}
	if pageCall["authorization"] != "api-key" {
		t.Fatalf("auth = %#v", pageCall["authorization"])
	}
	updateQuery := strings.Join(strings.Fields(calls[1]["query"].(string)), " ")
	if !strings.Contains(updateQuery, "issueUpdate(id: $id, input: { description: $description })") {
		t.Fatalf("update query = %s", updateQuery)
	}
}

func handlePageRequest(t *testing.T, calls *[]map[string]any, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	var request graphQLRequestBody
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		t.Errorf("decode request: %v", err)
		return
	}
	*calls = append(*calls, map[string]any{"query": request.Query, "variables": request.Variables, "authorization": r.Header.Get("Authorization")})
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.Contains(request.Query, "TeamIssuesPage"):
		_, err := io.WriteString(w, `{"data":{"issues":{"nodes":[{"identifier":"SEA-1","title":"t","url":"u","description":"d","state":{"name":"In Progress","type":"started"},"parent":{"identifier":"SEA-0"},"completedAt":"2026-08-20T10:00:00.000Z"},{"identifier":"SEA-2","title":"t2","url":"u2","state":null,"parent":null}],"pageInfo":{"hasNextPage":false,"endCursor":"c9"}}}}`)
		if err != nil {
			t.Errorf("write page: %v", err)
		}
	case strings.Contains(request.Query, "issueUpdate"):
		if request.Variables["description"] != "new body" {
			t.Errorf("update variables = %#v", request.Variables)
		}
		_, err := io.WriteString(w, `{"data":{"issueUpdate":{"success":true}}}`)
		if err != nil {
			t.Errorf("write update: %v", err)
		}
	default:
		t.Errorf("unexpected query: %s", request.Query)
	}
}

func TestIssueClientFetchIssueAndUpdateFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request graphQLRequestBody
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(request.Query, "issueUpdate") {
			_, err := io.WriteString(w, `{"data":{"issueUpdate":{"success":false}}}`)
			if err != nil {
				t.Errorf("write update response: %v", err)
			}
			return
		}
		_, err := io.WriteString(w, `{"data":{"issue":{"identifier":"RIG-2","title":"t","url":"u","description":null,"state":{"name":"Done","type":"completed"},"parent":{"identifier":"RIG-1"},"completedAt":"2026-08-20T10:00:00.000Z"}}}`)
		if err != nil {
			t.Errorf("write issue response: %v", err)
		}
	}))
	defer server.Close()
	api, err := NewIssueAPI(t.Context(), server.Client(), server.URL, func(name string) string {
		if name == "LINEAR_API_KEY" {
			return "api-key"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	fields, err := api.FetchIssue(t.Context(), "RIG-2")
	if err != nil {
		t.Fatal(err)
	}
	if fields.Identifier != "RIG-2" || fields.StateType != "completed" || fields.ParentIdentifier != "RIG-1" || fields.CompletedAt == nil || *fields.CompletedAt != "2026-08-20T10:00:00.000Z" {
		t.Fatalf("issue fields = %#v", fields)
	}
	err = api.UpdateDescription(t.Context(), "RIG-2", "not saved")
	if err == nil || err.Error() != "issueUpdate returned success=false for RIG-2" {
		t.Fatalf("update error = %v", err)
	}
}
