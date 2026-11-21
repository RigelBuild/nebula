package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReviewCommentPagination(t *testing.T) {
	sha := strings.Repeat("a", 40)
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		if r.URL.Query().Get("page") == "1" {
			entries := make([]map[string]any, 100)
			for i := range entries {
				entries[i] = map[string]any{"body": "round"}
			}
			respondJSON(t, w, entries)
			return
		}
		if r.URL.Query().Get("page") != "2" {
			t.Errorf("unexpected page %s", r.URL.RawQuery)
		}
		respondJSON(t, w, []any{map[string]any{"body": FormatMarker(Marker{Kind: markerAllClear, Reviewed: sha, Base: "main"}), "user": map[string]string{"login": "agent"}, "html_url": "page-two"}})
	}))
	defer server.Close()
	f := &forgeClient{client: server.Client(), baseURL: server.URL}
	markers, err := f.comments(t.Context(), "owner/repo", 1)
	if err != nil || pages != 2 || len(markers) != 1 || markers[0].URL != "page-two" || markers[0].Author != "agent" {
		t.Fatalf("pages %d markers %v err %v", pages, markers, err)
	}
}

func TestMarkReadyRejectsDraftReadback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/graphql" {
			respondJSON(t, w, map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]string{"id": "node"}}}})
			return
		}
		respondJSON(t, w, map[string]any{"draft": true})
	}))
	defer server.Close()
	f := &forgeClient{client: server.Client(), baseURL: server.URL}
	if err := f.markReady(t.Context(), "owner/repo", 1); err == nil || !strings.Contains(err.Error(), "remains draft") {
		t.Fatalf("error %v", err)
	}
}
