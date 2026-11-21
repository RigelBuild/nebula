package main

import (
	"strings"
	"testing"
)

func TestMainRedTrackingTrunkWorkspace(t *testing.T) {
	f := trackingFixture(t, "main", false)
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", "rigel-release@origin", "-T", "commit_id")
	base := jjTest(t, f.repo, "log", "--no-graph", "-r", "main@origin", "-T", "commit_id")
	if tip == base {
		t.Fatal("fixture trunk and mirror must differ")
	}
	t.Setenv("RJC_TEST_RECHECK_STATUS", "failed")
	rec := HookRecord{Result: hookPartial, SkippedTargets: []SkippedTarget{{Target: "a:test"}}}
	passed, err := f.repo.recheckMainRed(f.ctx, f.repo.profile, rec, tip)
	if err != nil || len(passed) != 0 {
		t.Fatalf("passed %v, error %v", passed, err)
	}
	if workspaces := jjTest(t, f.repo, "workspace", "list"); strings.Contains(workspaces, "rjc-main-red-") {
		t.Fatalf("workspace leaked: %s", workspaces)
	}
}
