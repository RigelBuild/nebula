package main

import (
	"io"
	"strings"
	"testing"
)

func TestSubmitHooksBeforePush(t *testing.T) {
	for _, tc := range []struct {
		name, phase, gate string
		fail              bool
	}{
		{name: "pass", phase: "runner-completed"},
		{name: "fail", phase: "runner-completed", fail: true, gate: "hooks-failed"},
		{name: "absent", phase: "no-config", gate: "hooks-absent"},
		{name: "setup", phase: "setup-failed", gate: "tool-failure"},
		{name: "fixup", phase: "fixup", gate: "hooks-fixup"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fixtureRepo(t)
			bookmark := f.new(t, "EX-1", "feat(rjc): add submit test", "")
			f.commit(t, "hook-change.txt")
			t.Setenv("RJC_TEST_HOOK_PHASE", tc.phase)
			if tc.fail {
				t.Setenv("RJC_TEST_HOOK_FAIL", "1")
			}
			err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: "feat(rjc): add submit test (EX-1)", bodyFile: f.bodyFile}, f, io.Discard)
			if tc.gate != "" {
				if err == nil || !strings.Contains(err.Error(), tc.gate) || len(f.pushes) != 0 {
					t.Fatalf("err=%v pushes=%v", err, f.pushes)
				}
				return
			}
			if err != nil || len(f.pushes) != 1 || len(f.noHooks) != 1 || !f.noHooks[0] {
				t.Fatalf("err=%v pushes=%v noHooks=%v", err, f.pushes, f.noHooks)
			}
		})
	}
}

func TestSubmitRigelProfileSkipsAbsentHooks(t *testing.T) {
	f := fixtureRepo(t)
	profile, err := ProfileFor(loadExampleConfig(t), "https://github.com/example/factory.git")
	if err != nil {
		t.Fatal(err)
	}
	f.repo.profile = profile
	bookmark := f.new(t, "EX-10107", "fix(rjc): submit Rigel PRs without hooks", "")
	f.commit(t, "feature.txt")
	var log strings.Builder
	f.repo.log = &log
	opt := submitOptions{bookmark: bookmark, title: "fix(rjc): submit Rigel PRs without hooks (EX-10107)", bodyFile: f.bodyFile}
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(f.pushes) != 1 || len(f.prs) != 1 {
		t.Fatalf("pushes=%v PRs=%v", f.pushes, f.prs)
	}
	if got := f.metadata[f.prs[bookmark]].Body; !strings.Contains(got, "Checks skipped: `"+profile.NoHooksReason+"`") {
		t.Fatalf("PR body does not record disabled-hook reason: %s", got)
	}
	if got := log.String(); !strings.Contains(got, "skip-checks: "+profile.NoHooksReason) {
		t.Fatalf("submit log does not record disabled-hook reason: %s", got)
	}
	store := localPrePush{root: f.repo.stateDir}
	changes, err := f.repo.submitPreflightLine(f.ctx, bookmark, jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id"), "", opt, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range changes {
		if blob, err := f.repo.hookConfigBlob(f.ctx, change.Tip, hkConfigName); err != nil || blob != "" {
			t.Fatalf("test commit unexpectedly has hook config: blob=%q err=%v", blob, err)
		}
		if _, found, err := store.Read(f.ctx, profile.Name, 0, change.Tip); err != nil || !found {
			t.Fatalf("recorded skip for %s: found=%v err=%v", change.Bookmark, found, err)
		}
	}
}

func TestSubmitJJVineProfileSkipsAbsentHooks(t *testing.T) {
	f := fixtureRepo(t)
	profile, err := ProfileFor(loadExampleConfig(t), "https://github.com/example/jj-vine.git")
	if err != nil {
		t.Fatal(err)
	}
	f.repo.profile = profile
	jjTest(t, f.repo, "bookmark", "create", "rigel-release", "-r", "main@origin")
	jjTest(t, f.repo, "git", "push", "--bookmark", "rigel-release")
	if err := f.repo.fetchMain(f.ctx); err != nil {
		t.Fatal(err)
	}
	bookmark := f.new(t, "EX-10545", "fix(rjc): submit jj-vine PRs without hooks", "")
	f.commit(t, "feature.txt")
	var log strings.Builder
	f.repo.log = &log
	opt := submitOptions{bookmark: bookmark, title: "fix(rjc): submit jj-vine PRs without hooks (EX-10545)", bodyFile: f.bodyFile}
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(f.pushes) != 1 || len(f.prs) != 1 {
		t.Fatalf("pushes=%v PRs=%v", f.pushes, f.prs)
	}
	reason := profile.NoHooksReason
	if got := f.metadata[f.prs[bookmark]].Body; !strings.Contains(got, "Checks skipped: `"+reason+"`") {
		t.Fatalf("PR body does not record disabled-hook reason: %s", got)
	}
	if got := log.String(); !strings.Contains(got, "skip-checks: "+reason) {
		t.Fatalf("submit log does not record disabled-hook reason: %s", got)
	}
}

func TestSubmitRigelProfileRefusesAbsentEnabledHooks(t *testing.T) {
	f := fixtureRepo(t)
	profile, err := ProfileFor(loadExampleConfig(t), "https://github.com/example/factory.git")
	if err != nil {
		t.Fatal(err)
	}
	profile.Hooks = true
	f.repo.profile = profile
	bookmark := f.new(t, "EX-10107", "fix(rjc): submit Rigel PRs without hooks", "")
	f.commit(t, "feature.txt")
	t.Setenv("RJC_TEST_HOOK_PHASE", "no-config")
	opt := submitOptions{bookmark: bookmark, title: "fix(rjc): submit Rigel PRs without hooks (EX-10107)", bodyFile: f.bodyFile}
	err = f.repo.submit(f.ctx, opt, f, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "hooks-absent") || len(f.pushes) != 0 {
		t.Fatalf("err=%v pushes=%v", err, f.pushes)
	}
}
