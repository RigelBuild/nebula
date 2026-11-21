package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRetireRefusesOpenAndNoPRWithExistingPR(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): retirement", "")
	f.prs[bookmark] = 1
	if err := (Ledger{root: f.repo.stateDir}).Save(LedgerEntry{Repo: f.repo.profile.Name, PR: 1, Bookmark: bookmark, Codename: f.repo.codename}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/pulls/1") {
			respondJSON(t, w, map[string]any{"state": "open", "head": map[string]string{"ref": bookmark}})
			return
		}
		respondJSON(t, w, []map[string]any{{"number": 1, "head": map[string]string{"ref": bookmark}}})
	}))
	defer server.Close()
	s, _ := service(f.ctx)
	s.forge = &forgeClient{client: server.Client(), baseURL: server.URL}
	for _, noPR := range []bool{false, true} {
		if err := f.repo.retire(f.ctx, retireOptions{bookmark: bookmark, noPR: noPR}, io.Discard); err == nil {
			t.Fatalf("noPR=%v: expected refusal", noPR)
		}
	}
}

func TestRetirePlanValidation(t *testing.T) {
	for _, tc := range []struct {
		plan  string
		valid bool
	}{
		{"Changes to push to origin:\n  bookmark: test-lane/ex-1-retirement [delete from abc]\n", true},
		{"Changes to push to origin:\n  bookmark: test-lane/ex-1-retirement [delete from abc]\n  bookmark: test-lane/ex-2-other [delete from abc]\n", false},
		{"Changes to push to origin:\n  bookmark: test-lane/ex-1-retirement [add to abc]\n", false},
	} {
		if got := retirementPlanValid(tc.plan, "test-lane/ex-1-retirement"); got != tc.valid {
			t.Errorf("plan %q: got %v", tc.plan, got)
		}
	}
}
