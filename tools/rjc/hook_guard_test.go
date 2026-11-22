package main

import (
	"strings"
	"testing"
)

// inheritFixtureFile is inheritFixture with the upper head's increment set to file instead of upper.txt, so a test
// can choose exactly which path the upper head changes. The upper head has no own record, so it may inherit.
func inheritFixtureFile(t *testing.T, f *fixture, file string) (localPrePush, []Change) {
	t.Helper()
	gateModuleFixture(t, f)
	return inheritFixtureNoModule(t, f, file)
}

// inheritFixtureNoModule builds the same heads without a gate module, so go list cannot derive a closure.
func inheritFixtureNoModule(t *testing.T, f *fixture, file string) (localPrePush, []Change) {
	t.Helper()
	lower := f.new(t, "EX-1", "feat(rjc): inherit guard fixture", "")
	f.commit(t, "lower.txt")
	jjTest(t, f.repo, "bookmark", "move", lower, "--to", "@-")
	lowerTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(lower), "-T", "commit_id")
	f.commit(t, file)
	upper := "test-lane/inherit-guard"
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
	return store, []Change{lowerChange, upperChange}
}

// H1: an upper increment that adds any hk discovery path must refuse inheritance. hk searches these paths, so a
// committed override there would replace the lower head's hook config when the upper head is rechecked.
// H2: an upper increment that touches the gate runner or any hook config must refuse inheritance. Names match
// case-insensitively (APFS, NTFS), so GO.WORK or TOOLS/GATE-RUNNER/x.go refuses too.
func TestInheritRefusesRiskyIncrement(t *testing.T) {
	t.Parallel()
	for _, file := range []string{
		"hk.local.pkl",
		".config/hk.local.pkl",
		".config/hk.pkl",
		"hk.pkl",
		"lefthook.yml",
		".pre-commit-config.yaml",
		"prek.toml",
		"tools/gate-runner/x.go",
		"go.mod",
		"go.sum",
		"go.work",
		"go.work.sum",
		"vendor/example.test/dep/dep.go",
		"tools/gate-runner/vendor/example.test/dep/dep.go",
		"GO.WORK",
		"Go.Work.Sum",
		"Vendor/modules.txt",
		"TOOLS/GATE-RUNNER/x.go",
		"libs/other/vendor/x.go",
	} {
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			f := parallelFixtureRepo(t)
			stubIssueState(t, f)
			store, line := inheritFixtureFile(t, f, file)
			err := f.repo.checkSkip(f.ctx, line, "main-red:#7", store)
			if err == nil || !strings.Contains(err.Error(), "skip-evidence") {
				t.Fatalf("increment adding %s was inherited: %v", file, err)
			}
		})
	}
}

// inheritFixtureWith is inheritFixtureFile with the upper increment made by write, so a test can rename or edit.
func inheritFixtureWith(t *testing.T, f *fixture, write func(t *testing.T, f *fixture)) (localPrePush, []Change) {
	t.Helper()
	lower := f.new(t, "EX-1", "feat(rjc): inherit closure fixture", "")
	f.commit(t, "lower.txt")
	jjTest(t, f.repo, "bookmark", "move", lower, "--to", "@-")
	lowerTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(lower), "-T", "commit_id")
	write(t, f)
	upper := "test-lane/inherit-closure"
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
	return store, []Change{lowerChange, upperChange}
}

// H2 control: an unrelated upper increment still inherits, so the refusal above is specific to risky paths.
func TestInheritAllowsUnrelatedIncrement(t *testing.T) {
	f := fixtureRepo(t)
	stubIssueState(t, f)
	store, line := inheritFixtureFile(t, f, "docs/readme.txt")
	if err := f.repo.checkSkip(f.ctx, line, "main-red:#7", store); err != nil {
		t.Fatalf("unrelated increment refused: %v", err)
	}
}
