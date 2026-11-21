package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

type lifecycleFixture struct {
	f                 *fixture
	bookmark, head    string
	comments          []map[string]any
	posts, promotions int
}

func newLifecycleFixture(t *testing.T) *lifecycleFixture {
	t.Helper()
	f := fixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): lifecycle test", "")
	f.commit(t, "change.txt")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: "feat(rjc): lifecycle test (EX-1)", bodyFile: f.bodyFile, draft: true}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	l := &lifecycleFixture{f: f, bookmark: bookmark, head: f.metadata[f.prs[bookmark]].HeadSHA}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			respondJSON(t, w, map[string]string{"login": "agent"})
		case strings.HasSuffix(r.URL.Path, "/status"):
			respondJSON(t, w, map[string]string{"state": "failure"})
		case r.URL.Path == "/repos/"+f.repo.profile.Name+"/issues/1":
			respondJSON(t, w, map[string]string{"state": "closed"})
		case strings.HasSuffix(r.URL.Path, "/comments"):
			if r.Method == http.MethodGet {
				respondJSON(t, w, l.comments)
				return
			}
			var body struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			l.posts++
			l.comments = append(l.comments, map[string]any{"body": body.Body, "user": map[string]string{"login": "agent"}, "html_url": "comment"})
			respondJSON(t, w, map[string]string{"html_url": "comment"})
		case r.URL.Path == "/graphql":
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
			l.promotions++
			meta := f.metadata[f.prs[bookmark]]
			meta.Draft = false
			f.metadata[f.prs[bookmark]] = meta
			respondJSON(t, w, map[string]any{"data": map[string]any{"markPullRequestReadyForReview": map[string]any{"pullRequest": map[string]bool{"isDraft": false}}}})
		default:
			f.serve(t, w, r)
		}
	}))
	t.Cleanup(server.Close)
	s, err := service(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.forge = &forgeClient{client: server.Client(), baseURL: server.URL}
	return l
}

func (l *lifecycleFixture) command(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out, stderr bytes.Buffer
	code := run(l.f.ctx, args, &out, &stderr)
	return code, out.String() + stderr.String()
}
func (l *lifecycleFixture) clear(author, head, base string) {
	l.comments = append(l.comments, map[string]any{"body": FormatMarker(Marker{Kind: markerAllClear, Reviewed: head, Base: base}), "user": map[string]string{"login": author}, "html_url": "comment"})
}

func TestLifecycleClearPromotesDraftAndReadyIsIdempotent(t *testing.T) {
	l := newLifecycleFixture(t)
	if code, out := l.command(t, "note", l.bookmark, "--clear", "--reviewed", strings.Repeat("a", 40)); code != 1 || !strings.Contains(out, "head-mismatch") {
		t.Fatalf("code %d %s", code, out)
	}
	if code, out := l.command(t, "ready", l.bookmark); code != 1 || !strings.Contains(out, "all-clear") {
		t.Fatalf("code %d %s", code, out)
	}
	if code, out := l.command(t, "note", l.bookmark, "--clear", "--reviewed", l.head); code != 0 {
		t.Fatalf("code %d %s", code, out)
	}
	if l.posts != 1 || l.promotions != 1 || l.f.metadata[1].Draft {
		t.Fatalf("posts=%d promotions=%d draft=%v", l.posts, l.promotions, l.f.metadata[1].Draft)
	}
	s, err := service(l.f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		State string `json:"state"`
	}
	if err := s.forge.api(l.f.ctx, http.MethodGet, "/repos/"+l.f.repo.profile.Name+"/commits/"+l.head+"/status", nil, &status); err != nil || status.State != "failure" {
		t.Fatalf("red CI fixture: %+v %v", status, err)
	}
	ledgerPath, err := (Ledger{root: l.f.repo.stateDir}).path(l.f.repo.profile.Name, 1)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if code, out := l.command(t, "ready", l.bookmark); code != 0 || l.promotions != 1 || !strings.Contains(out, "already-ready") {
		t.Fatalf("code %d %s", code, out)
	}
	if code, out := l.command(t, "note", l.bookmark, "--clear", "--reviewed", l.head); code != 0 || l.posts != 1 || l.promotions != 1 {
		t.Fatalf("already-ready clear changed state: code=%d posts=%d promotions=%d output=%s", code, l.posts, l.promotions, out)
	}
	after, err := os.ReadFile(ledgerPath)
	if err != nil || !bytes.Equal(before, after) || l.posts != 1 || l.promotions != 1 {
		t.Fatalf("already-ready mutated state: %v", err)
	}
}
func TestLifecycleClearRetryPromotesExistingMarker(t *testing.T) {
	l := newLifecycleFixture(t)
	priorityState := "completed"
	l.f.repo.lookupLinearPriority = func(_ context.Context, issue string) (int, string, bool, error) {
		if issue != "EX-9887" {
			t.Fatalf("looked up issue %q, want EX-9887", issue)
		}
		return 1, priorityState, true, nil
	}
	store := localPrePush{root: l.f.repo.stateDir}
	rec, found, err := store.Read(l.f.ctx, l.f.repo.profile.Name, 1, l.head)
	if err != nil || !found {
		t.Fatalf("record: found=%v error=%v", found, err)
	}
	rec.Result, rec.Skip = hookSkipped, "p0:EX-9887"
	if err := store.Save(rec); err != nil {
		t.Fatal(err)
	}
	args := []string{"note", l.bookmark, "--clear", "--reviewed", l.head}
	if code, out := l.command(t, args...); code != 1 || !strings.Contains(out, "hooks-skipped") || !strings.Contains(out, "all-clear posted at comment; PR left as draft") {
		t.Fatalf("code=%d output=%s, want P0 refusal with all-clear draft guidance", code, out)
	}
	if l.posts != 1 || l.promotions != 0 || !l.f.metadata[1].Draft {
		t.Fatalf("posts=%d promotions=%d draft=%v", l.posts, l.promotions, l.f.metadata[1].Draft)
	}
	priorityState = "started"
	if code, out := l.command(t, args...); code != 0 || l.posts != 1 || l.promotions != 1 || l.f.metadata[1].Draft {
		t.Fatalf("retry changed state unexpectedly: code=%d posts=%d promotions=%d draft=%v output=%s", code, l.posts, l.promotions, l.f.metadata[1].Draft, out)
	}
	if code, out := l.command(t, args...); code != 0 || l.posts != 1 || l.promotions != 1 || l.f.metadata[1].Draft {
		t.Fatalf("third run changed state: code=%d posts=%d promotions=%d draft=%v output=%s", code, l.posts, l.promotions, l.f.metadata[1].Draft, out)
	}
}

func TestLifecycleClearP0SkipRequiresOpenUrgentIssue(t *testing.T) {
	tests := []struct {
		name        string
		priority    int
		state       string
		wantRefusal bool
	}{
		{name: "open urgent", priority: 1, state: "started"},
		{name: "closed urgent", priority: 1, state: "completed", wantRefusal: true},
		{name: "open high", priority: 2, state: "started", wantRefusal: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := newLifecycleFixture(t)
			l.f.repo.lookupLinearPriority = func(_ context.Context, issue string) (int, string, bool, error) {
				if issue != "EX-9887" {
					t.Fatalf("looked up issue %q, want EX-9887", issue)
				}
				return tc.priority, tc.state, true, nil
			}
			store := localPrePush{root: l.f.repo.stateDir}
			rec, found, err := store.Read(l.f.ctx, l.f.repo.profile.Name, 1, l.head)
			if err != nil || !found {
				t.Fatalf("record: found=%v error=%v", found, err)
			}
			rec.Result, rec.Skip = hookSkipped, "p0:EX-9887"
			if err := store.Save(rec); err != nil {
				t.Fatal(err)
			}
			code, out := l.command(t, "note", l.bookmark, "--clear", "--reviewed", l.head)
			if tc.wantRefusal {
				if code != 1 || !strings.Contains(out, "hooks-skipped") {
					t.Fatalf("code=%d output=%s, want hooks-skipped refusal", code, out)
				}
				if l.posts != 1 || l.promotions != 0 || !l.f.metadata[1].Draft {
					t.Fatalf("posts=%d promotions=%d draft=%v, want posted note, no promotion, and draft", l.posts, l.promotions, l.f.metadata[1].Draft)
				}
				return
			}
			if code != 0 || l.posts != 1 || l.promotions != 1 || l.f.metadata[1].Draft {
				t.Fatalf("code=%d posts=%d promotions=%d draft=%v output=%s", code, l.posts, l.promotions, l.f.metadata[1].Draft, out)
			}
		})
	}
}

func TestLifecycleClearLeavesIneligibleDraft(t *testing.T) {
	l := newLifecycleFixture(t)
	store := localPrePush{root: l.f.repo.stateDir}
	rec, found, err := store.Read(l.f.ctx, l.f.repo.profile.Name, 1, l.head)
	if err != nil || !found {
		t.Fatalf("record: found=%v error=%v", found, err)
	}
	rec.Result = hookFail
	if err := store.Save(rec); err != nil {
		t.Fatal(err)
	}
	code, out := l.command(t, "note", l.bookmark, "--clear", "--reviewed", l.head)
	if code != 1 || !strings.Contains(out, "hooks-not-passed") {
		t.Fatalf("code=%d output=%s, want hooks-not-passed refusal", code, out)
	}
	if !l.f.metadata[1].Draft || l.promotions != 0 || l.posts != 1 {
		t.Fatalf("draft=%v promotions=%d posts=%d, want draft, no promotion, and posted note", l.f.metadata[1].Draft, l.promotions, l.posts)
	}
}

func TestAlreadyReadyRejectsMovedHeadOrBase(t *testing.T) {
	for _, moved := range []string{"head", "base"} {
		t.Run(moved, func(t *testing.T) {
			l := newLifecycleFixture(t)
			l.clear("agent", l.head, l.f.repo.profile.Trunk)
			meta := l.f.metadata[1]
			meta.Draft = false
			if moved == "head" {
				meta.HeadSHA = strings.Repeat("a", 40)
			} else {
				meta.BaseRef = "other"
			}
			l.f.metadata[1] = meta
			ledgerPath, err := (Ledger{root: l.f.repo.stateDir}).path(l.f.repo.profile.Name, 1)
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(ledgerPath)
			if err != nil {
				t.Fatal(err)
			}
			code, out := l.command(t, "ready", l.bookmark)
			after, err := os.ReadFile(ledgerPath)
			if code != 1 || err != nil || !bytes.Equal(before, after) || l.posts != 0 || l.promotions != 0 {
				t.Fatalf("code=%d output=%s read=%v posts=%d promotions=%d", code, out, err, l.posts, l.promotions)
			}
		})
	}
}
