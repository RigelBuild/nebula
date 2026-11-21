package main

import (
	"strings"
	"testing"
)

func TestSkipRejectsOlderHead(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	f.commit(t, "first.txt")
	old := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	line := []Change{{Bookmark: "test-lane/feature", Tip: old, Base: mainOrigin, Owned: true}}
	store := localPrePush{root: f.repo.stateDir}
	id, err := f.repo.hookIdentity(f.ctx, line[0])
	if err != nil {
		t.Fatal(err)
	}
	id.Result = hookFail
	if err := store.Save(id); err != nil {
		t.Fatal(err)
	}
	f.commit(t, "second.txt")
	line[0].Tip = jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	stubIssueState(t, f)
	if err := f.repo.checkSkip(f.ctx, line, "main-red:#1", store); err == nil || !strings.Contains(err.Error(), "skip-evidence") {
		t.Fatalf("old head accepted: %v", err)
	}
}
