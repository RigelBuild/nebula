package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTrackingSubmitCreatesPRAgainstTrunk(t *testing.T) {
	t.Parallel()
	f := trackingFixture(t, "main", false)
	bookmark := f.new(t, "EX-1", "feat(rjc): create against release trunk", "")
	f.commit(t, "tracking-feature.txt")
	server := trackingCreatePRServer(t, f)
	defer server.Close()
	configureTrackingVine(t, f, server)

	opt := submitOptions{bookmark: bookmark, title: "feat(rjc): create against release trunk", bodyFile: f.bodyFile}
	var log bytes.Buffer
	if err := f.repo.submit(f.ctx, opt, vine{repo: f.repo}, &log); err != nil {
		t.Fatalf("submit through jj-vine: %v\n%s", err, log.String())
	}

	pr := f.prs[bookmark]
	meta := f.metadata[pr]
	if pr == 0 || meta.BaseRef != f.repo.profile.Trunk {
		t.Fatalf("created PR base = %q, want trunk %q", meta.BaseRef, f.repo.profile.Trunk)
	}
}

func TestTrackingReadyRefusesMirrorBasedPR(t *testing.T) {
	t.Parallel()
	l := newLifecycleFixture(t)
	f := l.f
	jjTest(t, f.repo, "bookmark", "create", "rigel-release", "-r", "main@origin")
	jjTest(t, f.repo, "git", "push", "--bookmark", "rigel-release")
	f.repo.profile.Posture = forkTracking
	f.repo.profile.Trunk = "rigel-release"
	f.repo.profile.Mirror = "main"
	meta := f.metadata[1]
	meta.BaseRef = f.repo.profile.Mirror
	f.metadata[1] = meta

	code, out := l.command(t, "ready", l.bookmark)
	if code != 1 || l.promotions != 0 || !strings.Contains(out, "fork-base-mismatch") {
		t.Fatalf("code=%d output=%s promotions=%d", code, out, l.promotions)
	}
	if !f.metadata[1].Draft {
		t.Fatal("mirror-based PR was marked ready")
	}
}

func trackingCreatePRServer(t *testing.T, f *fixture) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/graphql" {
			respondJSON(t, w, map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequests": map[string]any{"nodes": []any{}}}}})
			return
		}
		if req.URL.Path == "/repos/"+f.repo.profile.Name+"/pulls" && req.Method == http.MethodPost {
			serveCreatedTrackingPR(t, f, w, req)
			return
		}
		if req.URL.Path == "/repos/"+f.repo.profile.Name+"/pulls/1" {
			serveTrackingPRMetadata(t, f, w, req)
			return
		}
		f.serve(t, w, req)
	}))
}

func serveTrackingPRMetadata(t *testing.T, f *fixture, w http.ResponseWriter, req *http.Request) {
	t.Helper()
	if req.Method == http.MethodPatch {
		var patch map[string]string
		if err := json.NewDecoder(req.Body).Decode(&patch); err != nil {
			t.Error(err)
			return
		}
		meta := f.metadata[1]
		if title, ok := patch["title"]; ok {
			meta.Title = title
		}
		if body, ok := patch["body"]; ok {
			meta.Body = body
		}
		f.metadata[1] = meta
	}
	respondJSON(t, w, decorateVinePR(1, f.metadata[1]))
}

func serveCreatedTrackingPR(t *testing.T, f *fixture, w http.ResponseWriter, req *http.Request) {
	t.Helper()
	var request struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Draft bool   `json:"draft"`
	}
	if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
		t.Error(err)
		return
	}
	head, err := f.repo.one(req.Context(), quoteRev(request.Head+"@origin"))
	if err != nil {
		t.Error(err)
		return
	}
	base, err := f.repo.one(req.Context(), quoteRev(request.Base+"@origin"))
	if err != nil {
		t.Error(err)
		return
	}
	f.prs[request.Head] = 1
	f.metadata[1] = Metadata{Title: request.Title, Body: request.Body, HeadRef: request.Head, HeadSHA: head, BaseRef: request.Base, BaseSHA: base, Draft: request.Draft}
	created := decorateVinePR(1, f.metadata[1])
	w.WriteHeader(http.StatusCreated)
	respondJSON(t, w, created)
}

func configureTrackingVine(t *testing.T, f *fixture, server *httptest.Server) {
	t.Helper()
	configureRealVine(t, f, server)
	s, err := service(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.forge = &forgeClient{baseURL: server.URL, client: server.Client()}
}
