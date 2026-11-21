package main

import (
	"strings"
	"testing"
)

func TestHookReportsOffRefusesSelectiveWork(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	err := f.repo.checkSkipTargets(f.ctx, nil, []SkippedTarget{{Target: "a:test", Reason: "EX-1"}}, localPrePush{root: f.repo.stateDir})
	if err == nil || !strings.Contains(err.Error(), "hook-reports-off") {
		t.Fatalf("skip target: %v", err)
	}
	rec := HookRecord{Result: hookSkipped, Skip: "main-red:#1", FailedGates: []string{"sea-ref"}}
	_, err = f.repo.recheckMainRed(f.ctx, f.repo.profile, rec, "")
	if err == nil || !strings.Contains(err.Error(), "hook-reports-off") {
		t.Fatalf("named gate: %v", err)
	}
}
