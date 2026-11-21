package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadGateStatePagesAndIncompleteCoverage(t *testing.T) {
	for _, incomplete := range []bool{false, true} {
		t.Run(map[bool]string{false: "all pages in position order", true: "incomplete pages conservatively covered"}[incomplete], func(t *testing.T) {
			pages := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/timeline") {
					respondJSON(t, w, githubMetadata(Metadata{Draft: true}))
					return
				}
				pages++
				if r.URL.Query().Get("page") == "1" {
					events := make([]map[string]any, 100)
					for i := range events {
						events[i] = map[string]any{"event": "committed", "id": i + 1}
					}
					respondJSON(t, w, events)
					return
				}
				if incomplete {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				respondJSON(t, w, []map[string]any{{"event": "reviewed", "state": "approved", "id": 101}})
			}))
			defer server.Close()
			ctx := withServices(t.Context(), &services{forge: &forgeClient{baseURL: server.URL, client: server.Client()}})
			state, err := ReadGateState(ctx, "example/app", 1)
			expected := Covered
			if incomplete {
				expected = Unknown
			}
			if err != nil || state.Coverage != expected || !state.Draft || pages != 2 {
				t.Fatalf("state=%+v pages=%d error=%v", state, pages, err)
			}
		})
	}
}
