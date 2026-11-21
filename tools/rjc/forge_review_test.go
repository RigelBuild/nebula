package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReviewForgeOperations(t *testing.T) {
	sha := strings.Repeat("a", 40)
	mutations := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			respondJSON(t, w, map[string]string{"login": "agent"})
		case "/repos/owner/repo/issues/1/comments":
			if r.Method == http.MethodPost {
				mutations++
				respondJSON(t, w, map[string]string{"html_url": "posted"})
				return
			}
			respondJSON(t, w, []any{map[string]any{"body": FormatMarker(Marker{Kind: markerAllClear, Reviewed: sha, Base: "main"}), "user": map[string]string{"login": "other"}, "html_url": "comment"}})
		case "/graphql":
			var body struct {
				Query string `json:"query"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			if strings.HasPrefix(body.Query, "query") {
				respondJSON(t, w, map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]string{"id": "node"}}}})
				return
			}
			mutations++
			respondJSON(t, w, map[string]any{"data": map[string]any{"markPullRequestReadyForReview": map[string]any{"pullRequest": map[string]bool{"isDraft": false}}}})
		case "/repos/owner/repo/pulls/1":
			respondJSON(t, w, map[string]any{"draft": false})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	f := &forgeClient{client: server.Client(), baseURL: server.URL}
	ctx := context.Background()
	viewer, err := f.viewer(ctx)
	if err != nil || viewer != "agent" {
		t.Fatalf("viewer %q %v", viewer, err)
	}
	markers, err := f.comments(ctx, "owner/repo", 1)
	if err != nil || len(markers) != 1 || markers[0].Author != "other" {
		t.Fatalf("markers %#v %v", markers, err)
	}
	if _, err = f.postComment(ctx, "owner/repo", 1, "body"); err != nil {
		t.Fatal(err)
	}
	if err = f.markReady(ctx, "owner/repo", 1); err != nil {
		t.Fatal(err)
	}
	if mutations != 2 {
		t.Fatalf("mutations %d", mutations)
	}
}
