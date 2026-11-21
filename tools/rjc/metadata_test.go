package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type memoryBaseline struct {
	value   Published
	commits int
}

func (s *memoryBaseline) Load(string) (Published, bool, error) { return s.value, true, nil }
func (s *memoryBaseline) Commit(_ string, p Published) error   { s.value = p; s.commits++; return nil }

type reconciliationCase struct {
	name                                                                        string
	human, humanDuring, mismatch, titleOnly, approval, draftChange, titleDuring bool
	headBaseChanged                                                             bool
	wantGate                                                                    string
}
type reconciliationHTTP struct {
	t              *testing.T
	tc             reconciliationCase
	current        Metadata
	patches, reads int
}

func (h *reconciliationHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/timeline"):
		events := []map[string]any{}
		if h.tc.approval {
			events = append(events, map[string]any{"event": "reviewed", "state": "approved", "id": 12})
		}
		respondJSON(h.t, w, events)
	case r.Method == http.MethodPatch:
		h.patch(w, r)
	default:
		h.reads++
		if h.tc.draftChange && h.reads >= 2 {
			h.current.Draft = true
		}
		if h.tc.humanDuring && h.reads >= 2 {
			h.current.Body = "human edit during gate-state read\n"
		}
		if h.tc.titleDuring && h.reads >= 2 {
			h.current.Title = "human title edit"
		}
		respondJSON(h.t, w, githubMetadata(h.current))
	}
}
func (h *reconciliationHTTP) patch(w http.ResponseWriter, r *http.Request) {
	h.patches++
	var payload map[string]string
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		h.t.Error(err)
	}
	if h.tc.titleOnly {
		if _, ok := payload["body"]; ok {
			h.t.Error("title-only PATCH sent body")
		}
	}
	if value, ok := payload["title"]; ok {
		h.current.Title = value
	}
	if value, ok := payload["body"]; ok && !h.tc.mismatch {
		h.current.Body = value
	}
	respondJSON(h.t, w, githubMetadata(h.current))
}

func TestReconcileMetadata(t *testing.T) {
	for _, tc := range []reconciliationCase{
		{name: "normal A to B"},
		{name: "read back mismatch", mismatch: true, wantGate: "read-back-mismatch"},
		{name: "human edit", human: true, wantGate: "human-body-edit"},
		{name: "human edit between snapshot and fresh read", humanDuring: true, wantGate: "human-body-edit"},
		{name: "title edit between snapshot and fresh read", titleDuring: true, wantGate: "remote-changed"},
		{name: "head and base differ from compose snapshot", headBaseChanged: true, wantGate: "head-base-changed"},
		{name: "non draft with no approval"},
		{name: "new approval", approval: true},
		{name: "draft transition", draftChange: true},
		{name: "title only", titleOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) { testReconciliation(t, tc) })
	}
}
func testReconciliation(t *testing.T, tc reconciliationCase) {
	t.Helper()
	bodyA, bodyB := "old authored body\n", "new authored body\n"
	block := "<!-- start jj-vine stack -->\nexact stack bytes\n<!-- end jj-vine stack -->"
	h := &reconciliationHTTP{t: t, tc: tc, current: Metadata{Title: "old title", Body: block + "\n\n" + bodyA, HeadRef: "home/ex-1-test", HeadSHA: "head", BaseRef: "main", BaseSHA: "base"}}
	if tc.human {
		h.current.Body = block + "\n\nhuman edit\n"
	}
	want := h.current
	want.Title = "new title"
	want.Body = bodyB
	if tc.titleOnly {
		want.Body = bodyA
	}
	if tc.headBaseChanged {
		want.HeadSHA = "compose-head"
		want.BaseSHA = "compose-base"
	}
	server := httptest.NewServer(h)
	defer server.Close()
	store := &memoryBaseline{value: Published{Title: "old title", Authored: bodyA}}
	ctx := withServices(t.Context(), &services{forge: &forgeClient{baseURL: server.URL, client: server.Client()}, store: store, bookmark: "home/ex-1-test"})
	err := ReconcileMetadata(ctx, "example/app", 1, store.value, "", want)
	refused := tc.wantGate != ""
	if refused {
		assertRefusalGate(t, err, tc.wantGate)
	} else if err != nil {
		t.Fatalf("error = %v", err)
	}
	if refused && store.commits != 0 {
		t.Fatal("failed write advanced baseline")
	}
	if !refused && store.commits != 1 {
		t.Fatal("confirmed write did not advance baseline")
	}
	if refused && !tc.mismatch && h.patches != 0 {
		t.Fatal("refusal sent PATCH")
	}
	if tc.human && ReconcileMetadata(ctx, "example/app", 1, store.value, hashAuthored("stale human edit\n"), want) == nil {
		t.Fatal("stale acknowledgement accepted")
	}
}

func githubMetadata(m Metadata) map[string]any {
	labels := make([]map[string]string, 0, len(m.Labels))
	for _, name := range m.Labels {
		labels = append(labels, map[string]string{"name": name})
	}
	return map[string]any{"title": m.Title, "body": m.Body, "draft": m.Draft, "head": map[string]string{"ref": m.HeadRef, "sha": m.HeadSHA}, "base": map[string]string{"ref": m.BaseRef, "sha": m.BaseSHA}, "labels": labels}
}

func TestReRequestIfMoved(t *testing.T) {
	for _, tc := range []struct {
		name             string
		covered          bool
		oldHead, newHead string
		calls            int
	}{
		{name: "approved push", covered: true, oldHead: "old", newHead: "new", calls: 2},
		{name: "title only", covered: true, oldHead: "old", newHead: "old"},
		{name: "unknown push", covered: true, oldHead: "old", newHead: "new", calls: 2},
		{name: "uncovered push", oldHead: "old", newHead: "new"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				if strings.HasSuffix(r.URL.Path, "/comments") && !strings.Contains(payload["body"].(string), tc.newHead) {
					t.Error("comment omits new head")
				}
				w.WriteHeader(http.StatusCreated)
			}))
			defer server.Close()
			ctx := withServices(t.Context(), &services{forge: &forgeClient{baseURL: server.URL, client: server.Client()}})
			if err := ReRequestIfMoved(ctx, "example-reviewer", "example/app", 1, tc.covered, tc.oldHead, tc.newHead); err != nil {
				t.Fatal(err)
			}
			if calls != tc.calls {
				t.Fatalf("calls = %d, want %d", calls, tc.calls)
			}
		})
	}
}
