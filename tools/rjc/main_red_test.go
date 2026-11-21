package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLeftOut(t *testing.T) {
	tests := []struct {
		name           string
		rec            HookRecord
		targets, gates int
		whole, fail    bool
	}{
		{name: "partial", rec: HookRecord{Result: hookPartial, SkippedTargets: []SkippedTarget{{Target: "a:test"}}}, targets: 1},
		{name: "gate", rec: HookRecord{Result: "skipped", Skip: "main-red:#1", FailedGates: []string{"sea-ref"}}, gates: 1},
		{name: "whole", rec: HookRecord{Result: "skipped", Skip: "main-red:#1"}, whole: true},
		{name: "inherited covers whole head", rec: HookRecord{Result: "skipped", Skip: "main-red:#1", CoveredBy: "lower", FailedTargets: []string{"a:test"}}, whole: true},
		{name: "p0 skip without failed run", rec: HookRecord{Result: hookSkipped, Skip: "p0:EX-1"}, fail: true},
		{name: "free-text", rec: HookRecord{Result: "skipped", Skip: "flaky runner"}, fail: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			targets, gates, whole, err := leftOut(tt.rec)
			if (err != nil) != tt.fail || len(targets) != tt.targets || len(gates) != tt.gates || whole != tt.whole {
				t.Fatalf("got %v %v %v %v", targets, gates, whole, err)
			}
		})
	}
}

func TestGateRecheckReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(path, []byte(`[{"name":"sea-ref","exit":1}]`), 0600); err != nil {
		t.Fatal(err)
	}
	failed, err := readRecheckGate(path, "sea-ref")
	if err != nil || !failed {
		t.Fatalf("got %v %v", failed, err)
	}
	if _, err = readRecheckGate(path, "other"); err == nil {
		t.Fatal("missing entry accepted")
	}
}

func TestRecheckGateStatuses(t *testing.T) {
	for _, tc := range []struct {
		name, report   string
		failed, refuse bool
	}{
		{"passed", `[{"name":"gate","exit":0}]`, false, false},
		{"failed", `[{"name":"gate","exit":1}]`, true, false},
		{"missing-status", `[{"name":"gate"}]`, false, true},
		{"invalid-status", `[{"name":"gate","exit":2}]`, false, true},
		{"skipped-reason", `[{"name":"gate","exit":0,"reason":"skipped opt-in"}]`, false, true},
		{"infrastructure-reason", `[{"name":"gate","exit":1,"reason":"runner unavailable"}]`, false, true},
		{"missing-gate", `[]`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "report.json")
			if err := os.WriteFile(path, []byte(tc.report), 0o600); err != nil {
				t.Fatal(err)
			}
			failed, err := readRecheckGate(path, "gate")
			if failed != tc.failed || (err != nil) != tc.refuse {
				t.Fatalf("failed=%v err=%v", failed, err)
			}
		})
	}
}

func TestRecheckTargetStatuses(t *testing.T) {
	for _, tc := range []struct {
		status         string
		failed, refuse bool
	}{
		{"passed", false, false}, {"cached", false, false}, {"cached-from-remote", false, false},
		{"failed", true, false}, {"aborted", false, true}, {"timed-out", false, true},
		{"invalid", false, true}, {"skipped", false, true}, {"", false, true}, {"unknown", false, true},
	} {
		t.Run(tc.status, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "report.json")
			report := `{"actions":[{"status":"` + tc.status + `","node":{"action":"run-task","params":{"target":"a:test"}}}]}`
			if err := os.WriteFile(path, []byte(report), 0o600); err != nil {
				t.Fatal(err)
			}
			failed, err := readRecheckTarget(path, "a:test")
			if failed != tc.failed || (err != nil) != tc.refuse {
				t.Fatalf("failed=%v err=%v", failed, err)
			}
		})
	}
}
