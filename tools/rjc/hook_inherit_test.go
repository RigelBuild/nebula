package main

import (
	"strings"
	"testing"
)

// inheritFixture builds a failed lower head and an upper head above it, with the upper head's own record set by
// the caller. It returns the store and both changes.
func inheritFixture(t *testing.T, f *fixture, withOwn bool, own HookRecord) (localPrePush, []Change) {
	t.Helper()
	gateModuleFixture(t, f)
	lower := f.new(t, "EX-1", "feat(rjc): inherit fixture", "")
	f.commit(t, "lower.txt")
	jjTest(t, f.repo, "bookmark", "move", lower, "--to", "@-")
	lowerTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(lower), "-T", "commit_id")
	f.commit(t, "upper.txt")
	upper := "test-lane/inherit-upper"
	jjTest(t, f.repo, "bookmark", "create", upper, "-r", "@-")
	upperTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(upper), "-T", "commit_id")
	store := localPrePush{root: f.repo.stateDir}
	lowerChange := Change{Bookmark: lower, Tip: lowerTip, Base: mainOrigin, Owned: true}
	upperChange := Change{Bookmark: upper, Tip: upperTip, Base: lower, Owned: true}
	lowerID, err := f.repo.hookIdentity(f.ctx, lowerChange)
	if err != nil {
		t.Fatal(err)
	}
	lowerID.Result, lowerID.Run = hookFail, "lower-run"
	lowerID.FailedTargets = []string{"a:test"}
	if err := store.Save(lowerID); err != nil {
		t.Fatal(err)
	}
	if withOwn {
		upperID, err := f.repo.hookIdentity(f.ctx, upperChange)
		if err != nil {
			t.Fatal(err)
		}
		if own.Repo == "" {
			own.Repo = upperID.Repo
		}
		if own.Head == "" {
			own.Head = upperID.Head
		}
		if own.Base == "" {
			own.Base = upperID.Base
		}
		if own.HookConfigBlob == "" {
			own.HookConfigBlob = upperID.HookConfigBlob
		}
		if err := store.Save(own); err != nil {
			t.Fatal(err)
		}
	}
	return store, []Change{lowerChange, upperChange}
}

// F2: an upper head with its own record that is not a current failure must refuse inheritance. Each kind of own
// run is refused, so a head that ran never reuses the lower head's failure.
func TestInheritRefusesUpperHeadWithOwnRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		own  HookRecord
	}{
		{name: "own pass", own: HookRecord{Result: hookPass}},
		{name: "own partial", own: HookRecord{Result: hookPartial, SkippedTargets: []SkippedTarget{{Target: "c:real", Reason: "EX-7"}}}},
		{name: "own non-current fail", own: HookRecord{Result: hookFail, Run: "stale-run", FailedTargets: []string{"c:real"}, HookConfigBlob: "stale"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fixtureRepo(t)
			stubIssueState(t, f)
			store, line := inheritFixture(t, f, true, tc.own)
			err := f.repo.checkSkip(f.ctx, line, "main-red:#7", store)
			if err == nil || !strings.Contains(err.Error(), "head has its own hook run") {
				t.Fatalf("upper head with %s was covered by the lower failure: %v", tc.name, err)
			}
		})
	}
}

// F1: an inherited record (CoveredBy set) is the lower head's evidence, not this head's own run. After
// `ready --main-red` the upper record's Latest is main-red with CoveredBy set. Resubmitting the same stack with the
// same main-red skip must be allowed.
func TestInheritResubmitAfterMainRedAllowed(t *testing.T) {
	f := fixtureRepo(t)
	stubIssueState(t, f)
	store, line := inheritFixture(t, f, false, HookRecord{})
	if err := f.repo.checkSkip(f.ctx, line, "main-red:#7", store); err != nil {
		t.Fatalf("first inherited skip refused: %v", err)
	}
	upper := line[1]
	id, err := f.repo.hookIdentity(f.ctx, upper)
	if err != nil {
		t.Fatal(err)
	}
	latest, found, err := store.Read(f.ctx, id.Repo, 0, id.Head)
	if err != nil || !found || latest.CoveredBy == "" {
		t.Fatalf("upper record was not inherited: %+v found=%v err=%v", latest, found, err)
	}
	// Simulate `ready --main-red` recording main-red on the inherited head.
	latest.Result, latest.MainTip = hookMainRed, "trunk"
	if err := store.Save(latest); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.checkSkip(f.ctx, line, "main-red:#7", store); err != nil {
		t.Fatalf("resubmit after ready --main-red was refused: %v", err)
	}
}
