package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSkipReasonsRechecked(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	f.repo.profile.HookReports = true
	f.commit(t, "feature.txt")
	head := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	line := []Change{{Bookmark: "test-lane/feature", Tip: head, Base: mainOrigin, Owned: true}}
	store := localPrePush{root: f.repo.stateDir}
	id, err := f.repo.hookIdentity(f.ctx, line[0])
	if err != nil {
		t.Fatal(err)
	}
	pair := SkippedTarget{Target: "a:test", Reason: "#12"}
	id.Result = hookPartial
	id.SkippedTargets = []SkippedTarget{pair}
	if err := store.Save(id); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		respondJSON(t, w, map[string]string{"state": "closed"})
	}))
	t.Cleanup(server.Close)
	s, err := service(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.forge = &forgeClient{client: server.Client(), baseURL: server.URL}
	for _, reason := range []string{"#12", "https://github.com/elsewhere/repo/issues/12", "nonsense"} {
		pair.Reason = reason
		err := f.repo.checkSkipTargets(f.ctx, line, []SkippedTarget{pair}, store)
		if err == nil || !strings.Contains(err.Error(), "skip-reason") {
			t.Fatalf("reason %s accepted: %v", reason, err)
		}
	}
	if err := f.repo.checkSkip(f.ctx, line, "main-red:#12", store); err == nil || !strings.Contains(err.Error(), "skip-reason") {
		t.Fatalf("closed main-red accepted: %v", err)
	}
}
