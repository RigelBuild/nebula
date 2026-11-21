package main

import (
	"context"
	"io"
	"strings"
	"testing"
)

type stageRunner struct {
	phase       HookPhase
	exit, calls int
}

func (s *stageRunner) Run(_ context.Context, base, head string, _ []SkippedTarget, _ io.Writer) (HookOutcome, RunRef, error) {
	s.calls++
	return HookOutcome{Phase: s.phase, Exit: s.exit, From: base, To: head, Fixup: strings.Repeat("f", 40)}, RunRef{}, nil
}

func TestHookStageClassifiesAndReuses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		phase   HookPhase
		exit    int
		result  HookResult
		failure string
	}{
		{name: "pass", phase: phaseCompleted, result: hookPass},
		{name: "fail", phase: phaseCompleted, exit: 1, result: hookFail, failure: "hooks-failed"},
		{name: "absent", phase: phaseNoConfig, result: hookAbsent, failure: "hooks-absent"},
		{name: "fixup", phase: phaseFixup, result: hookFixup, failure: "hooks-fixup"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := parallelFixtureRepo(t)
			f.commit(t, "change.txt")
			head := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
			line := []Change{{Bookmark: "test-lane/test", Tip: head, Base: "main@origin", Owned: true}}
			store := localPrePush{root: f.repo.stateDir}
			runner := &stageRunner{phase: tc.phase, exit: tc.exit}
			err := f.repo.runHooks(f.ctx, line, nil, runner, store)
			if tc.failure == "" && err != nil || tc.failure != "" && (err == nil || !strings.Contains(err.Error(), tc.failure)) {
				t.Fatalf("classification error: %v", err)
			}
			rec, found, err := store.Read(f.ctx, f.repo.profile.Name, 0, head)
			if err != nil || !found || rec.Result != tc.result {
				t.Fatalf("record %+v %v", rec, err)
			}
			if tc.result == hookPass {
				if err := f.repo.runHooks(f.ctx, line, nil, runner, store); err != nil || runner.calls != 1 {
					t.Fatalf("pass not reused: calls=%d err=%v", runner.calls, err)
				}
			}
		})
	}
}
