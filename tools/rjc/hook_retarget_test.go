package main

import (
	"context"
	"io"
	"strings"
	"testing"
)

type countingHooks struct{ bases []string }

func (c *countingHooks) Run(_ context.Context, base, head string, _ []SkippedTarget, _ io.Writer) (HookOutcome, RunRef, error) {
	c.bases = append(c.bases, base)
	return HookOutcome{Phase: phaseCompleted, From: base, To: head}, RunRef{}, nil
}

type affectedHooks struct {
	repo   *repository
	checks [][]string
}

func (a *affectedHooks) Run(ctx context.Context, base, head string, _ []SkippedTarget, _ io.Writer) (HookOutcome, RunRef, error) {
	out, err := a.repo.command(ctx, nil, nil, "git", "diff", "--name-only", base, head)
	if err != nil {
		return HookOutcome{}, RunRef{}, err
	}
	checks := []string{}
	for path := range strings.FieldsSeq(string(out)) {
		checks = append(checks, "check:"+path)
	}
	a.checks = append(a.checks, checks)
	return HookOutcome{Phase: phaseCompleted, From: base, To: head}, RunRef{}, nil
}

func TestRetargetInvalidatesHookBase(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	f.commit(t, "parent.txt")
	jjTest(t, f.repo, "bookmark", "create", "parent", "-r", "@-")
	f.commit(t, "head.txt")
	head := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	line := []Change{{Bookmark: "test-lane/retarget", Tip: head, Base: "parent", Owned: true}}
	store := localPrePush{root: f.repo.stateDir}
	runner := &countingHooks{}
	if err := f.repo.runHooks(f.ctx, line, nil, runner, store); err != nil {
		t.Fatal(err)
	}
	line[0].Base = "main@origin"
	if err := f.repo.runHooks(f.ctx, line, nil, runner, store); err != nil {
		t.Fatal(err)
	}
	if len(runner.bases) != 2 || runner.bases[0] == runner.bases[1] {
		t.Fatalf("retarget did not rerun: %v", runner.bases)
	}
}

func TestRetargetExposesParentAffectedCheck(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	f.commit(t, "parent.txt")
	jjTest(t, f.repo, "bookmark", "create", "parent", "-r", "@-")
	f.commit(t, "head.txt")
	head := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	line := []Change{{Bookmark: "test-lane/retarget", Tip: head, Base: "parent", Owned: true}}
	store := localPrePush{root: f.repo.stateDir}
	runner := &affectedHooks{repo: f.repo}
	if err := f.repo.runHooks(f.ctx, line, nil, runner, store); err != nil {
		t.Fatal(err)
	}
	line[0].Base = mainOrigin
	if err := f.repo.runHooks(f.ctx, line, nil, runner, store); err != nil {
		t.Fatal(err)
	}
	if len(runner.checks) != 2 || strings.Join(runner.checks[0], ",") != "check:head.txt" || strings.Join(runner.checks[1], ",") != "check:head.txt,check:parent.txt" {
		t.Fatalf("affected checks: %v", runner.checks)
	}
}
