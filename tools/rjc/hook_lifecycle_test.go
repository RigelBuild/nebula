package main

import (
	"io"
	"os"
	"testing"
)

func TestTrunkAdvanceReusesHookRecord(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	f.commit(t, "feature.txt")
	head := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	line := []Change{{Bookmark: "test-lane/feature", Tip: head, Base: mainOrigin, Owned: true}}
	store := localPrePush{root: f.repo.stateDir}
	runner := &countingHooks{}
	if err := f.repo.runHooks(f.ctx, line, nil, runner, store); err != nil {
		t.Fatal(err)
	}
	jjTest(t, f.repo, "new", mainOrigin)
	f.commit(t, "trunk.txt")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	if err := f.repo.runHooks(f.ctx, line, nil, runner, store); err != nil {
		t.Fatal(err)
	}
	if len(runner.bases) != 1 {
		t.Fatalf("trunk advance reran hooks: %v", runner.bases)
	}
}

func TestSubmitCreatesAndRestoresLedger(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): record PR ownership", "")
	f.commit(t, "feature.txt")
	opt := submitOptions{bookmark: bookmark, title: "feat(rjc): record PR ownership (EX-1)", bodyFile: f.bodyFile}
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	ledger := Ledger{root: f.repo.stateDir}
	entry, found, err := ledger.Find(f.repo.profile.Name, bookmark)
	if err != nil || !found || entry.PR != f.prs[bookmark] {
		t.Fatalf("ledger entry: %+v found=%v err=%v", entry, found, err)
	}
	path, err := ledger.path(entry.Repo, entry.PR)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	restored, found, err := ledger.Find(f.repo.profile.Name, bookmark)
	if err != nil || !found || restored.PR != entry.PR {
		t.Fatalf("resubmit ledger: %+v found=%v err=%v", restored, found, err)
	}
}
