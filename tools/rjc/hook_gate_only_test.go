package main

import (
	"strings"
	"testing"
)

func TestGateOnlyFailureDoesNotAuthorizeTarget(t *testing.T) {
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
	id.Result = hookFail
	id.FailedGates = []string{"gate-a"}
	if err := store.Save(id); err != nil {
		t.Fatal(err)
	}
	err = f.repo.checkSkipTargets(f.ctx, line, []SkippedTarget{{Target: "a:test", Reason: "EX-1"}}, store)
	if err == nil || !strings.Contains(err.Error(), "skip-target-not-failed") {
		t.Fatalf("gate authorized target: %v", err)
	}
}
