package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMainRedWholeHook(t *testing.T) {
	f := fixtureRepo(t)
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", mainOrigin, "-T", "commit_id")
	f.commit(t, "base.txt")
	base := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	args := filepath.Join(t.TempDir(), "args")
	t.Setenv("RJC_TEST_HK_ARGS", args)
	t.Setenv("RJC_TEST_HK_FAIL", "1")
	rec := HookRecord{Result: hookSkipped, Skip: "main-red:#1", Base: base}
	passed, err := f.repo.recheckMainRed(f.ctx, f.repo.profile, rec, tip)
	if err != nil || len(passed) != 0 {
		t.Fatalf("passed %v error %v", passed, err)
	}
	data, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "run\npre-push\n--from-ref\n"+base+"\n--to-ref\n"+tip+"\n"+base+"\n"+tip+"\n" {
		t.Fatalf("args %q", data)
	}
	t.Setenv("RJC_TEST_HK_FAIL", "0")
	passed, err = f.repo.recheckMainRed(f.ctx, f.repo.profile, rec, tip)
	if err != nil || len(passed) != 1 || passed[0] != "whole hook" {
		t.Fatalf("passing hook: %v %v", passed, err)
	}
	if workspaces := jjTest(t, f.repo, "workspace", "list"); strings.Contains(workspaces, "rjc-main-red-") {
		t.Fatalf("workspace leaked: %s", workspaces)
	}
}

func TestMainRedInheritedRunsUpperIncrement(t *testing.T) {
	f := fixtureRepo(t)
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", mainOrigin, "-T", "commit_id")
	f.commit(t, "lower.txt")
	lowerTip := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	f.commit(t, "upper.txt")
	upperTip := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	args := filepath.Join(t.TempDir(), "args")
	t.Setenv("RJC_TEST_HK_ARGS", args)
	t.Setenv("RJC_TEST_HK_FAIL", "0")
	rec := HookRecord{Result: hookSkipped, Skip: "main-red:#1", Head: upperTip, Base: mainOrigin, CoveredBy: lowerTip, FailedTargets: []string{"a:test"}}
	passed, err := f.repo.recheckMainRed(f.ctx, f.repo.profile, rec, tip)
	if err == nil || !strings.Contains(err.Error(), "the head's own increment passes the hook") || len(passed) != 0 {
		t.Fatalf("passing inherited recheck: passed %v error %v, want the own-increment pass refusal", passed, err)
	}
	data, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	want := "run\npre-push\n--from-ref\n" + lowerTip + "\n--to-ref\n" + upperTip + "\n" + lowerTip + "\n" + upperTip + "\n" + hkConfigName
	if string(data) != want {
		t.Fatalf("inherited recheck did not run the upper increment:\ngot  %q\nwant %q", data, want)
	}
}

// An inherited recheck that fails is decided end to end: the hk stub writes the moon report, then readMoonReport
// and acceptInheritedFailures decide it. A failing target is accepted only if the lower head's failed run named it.
func TestMainRedInheritedFailingRecheck(t *testing.T) {
	for _, tc := range []struct {
		name    string
		target  string
		failed  string
		wantErr string
	}{
		{name: "lower failed target fails again", target: "a:test", failed: "1"},
		{name: "new failing target refused", target: "b:test", failed: "1", wantErr: "upper head fails outside the lower failed targets: b:test"},
		{name: "failing hook with no failing target refused", target: "a:test", failed: "0", wantErr: "upper head hook failed with no failing target in the report"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fixtureRepo(t)
			tip := jjTest(t, f.repo, "log", "--no-graph", "-r", mainOrigin, "-T", "commit_id")
			f.commit(t, "lower.txt")
			lowerTip := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
			f.commit(t, "upper.txt")
			upperTip := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
			t.Setenv("RJC_TEST_HK_FAIL", "1")
			t.Setenv("RJC_TEST_HK_TARGET", tc.target)
			t.Setenv("RJC_TEST_HK_TARGET_FAILED", tc.failed)
			rec := HookRecord{Result: hookSkipped, Skip: "main-red:#7", Head: upperTip, Base: mainOrigin, CoveredBy: lowerTip, FailedTargets: []string{"a:test"}}
			passed, err := f.repo.recheckMainRed(f.ctx, f.repo.profile, rec, tip)
			if tc.wantErr == "" {
				if err != nil || len(passed) != 0 {
					t.Fatalf("accepted failure refused: passed %v error %v", passed, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("passed %v error %v, want %q", passed, err, tc.wantErr)
			}
		})
	}
}
