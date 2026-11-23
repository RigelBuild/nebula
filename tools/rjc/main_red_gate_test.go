package main

import (
	"strings"
	"testing"
)

func TestMainRedGateBuildFailureCleansWorkspace(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	f.repo.profile.HookReports = true
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", mainOrigin, "-T", "commit_id")
	rec := HookRecord{Result: hookSkipped, Skip: "main-red:#1", FailedGates: []string{"sea-ref"}}
	_, err := f.repo.recheckMainRed(f.ctx, f.repo.profile, rec, tip)
	if err == nil {
		t.Fatal("missing gate runner accepted")
	}
	workspaces := jjTest(t, f.repo, "workspace", "list")
	if strings.Contains(workspaces, "rjc-main-red-") {
		t.Fatalf("workspace leaked: %s", workspaces)
	}
}
