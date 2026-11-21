package main

import (
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncMovedTrunk(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): sync a moved trunk", "")
	f.commit(t, "child.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	jjTest(t, f.repo, "new", mainOrigin)
	f.commit(t, "upstream.txt")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	jjTest(t, f.repo, "new", quoteRev(bookmark))
	results, err := f.repo.sync(f.ctx, syncOptions{bookmark: bookmark}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %v", results)
	}
	if got := jjTest(t, f.repo, "log", "--no-graph", "-r", "main@origin & ::"+quoteRev(bookmark), "-T", "commit_id"); got == "" {
		t.Fatal("bookmark is not based on trunk")
	}
	if len(f.pushes) != 0 || len(f.reviewCalls) != 0 {
		t.Fatal("sync wrote to forge")
	}
}

func TestSyncUpstreamMergeMovedTrunk(t *testing.T) {
	f := trackingFixture(t, "main", false)
	bookmark, err := f.repo.upstreamSync(f.ctx, upstreamSyncOptions{issue: "EX-1"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	rec, _, err := (fileBaseline{root: f.repo.stateDir, keys: f.repo.keys}).LoadUpstream(bookmark)
	if err != nil {
		t.Fatal(err)
	}
	jjTest(t, f.repo, "new", "rigel-release@origin")
	writeTestFile(t, filepath.Join(f.repo.root, "release-only.txt"), "release advance\n")
	jjTest(t, f.repo, "commit", "-m", "chore: advance release")
	jjTest(t, f.repo, "bookmark", "move", "rigel-release", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "rigel-release")
	trunk := jjTest(t, f.repo, "log", "--no-graph", "-r", "rigel-release@origin", "-T", "commit_id")
	jjTest(t, f.repo, "new", quoteRev(bookmark))
	results, err := f.repo.sync(f.ctx, syncOptions{bookmark: bookmark}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("sync results: %v", results)
	}
	parents := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", `parents.map(|p| p.commit_id()).join("\n")`)
	if parents != strings.Join([]string{trunk, rec.Upstream}, "\n") {
		t.Fatalf("parents: %s", parents)
	}
	change := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "change_id")
	if change != rec.Change {
		t.Fatalf("change ID %s, want %s", change, rec.Change)
	}
}

func TestSyncUpstreamMergeFailurePrintsRestore(t *testing.T) {
	f := trackingFixture(t, "main", false)
	bookmark, err := f.repo.upstreamSync(f.ctx, upstreamSyncOptions{issue: "EX-1"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	old := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	change := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "change_id")
	jjTest(t, f.repo, "new", "rigel-release@origin")
	f.commit(t, "release-only.txt")
	jjTest(t, f.repo, "bookmark", "move", "rigel-release", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "rigel-release")
	jjTest(t, f.repo, "new", quoteRev(bookmark))
	_, err = f.repo.sync(f.ctx, syncOptions{bookmark: bookmark}, failureWriter{})
	detail, ok := errors.AsType[*failure](err)
	if !ok || !strings.Contains(detail.fix, "jj bookmark set "+bookmark) || !strings.Contains(detail.fix, "jj abandon ") {
		t.Fatalf("missing upstream recovery: %v", err)
	}
	if !strings.Contains(detail.fix, "jj bookmark set "+bookmark+" -r "+old+" --allow-backwards") {
		t.Fatalf("recovery lacks exact old tip: %s", detail.fix)
	}
	current := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "change_id")
	if current != change {
		t.Fatalf("rewritten change id %s, want %s", current, change)
	}
	plan := syncPlan{Segments: []syncSegment{{Bookmark: bookmark, Commits: []string{old}}}}
	runSyncRecovery(t, f, detail.fix, plan)
	if got := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id"); got != old {
		t.Fatalf("restored tip = %s, want %s", got, old)
	}
}

func TestSyncRefusesForeignDescendant(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): sync a foreign descendant", "")
	f.commit(t, "child.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	f.commit(t, "foreign.txt")
	jjTest(t, f.repo, "bookmark", "create", "other-lane/ex-2-foreign", "-r", "@-")
	before := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	beforeOp := jjTest(t, f.repo, "op", "log", "--no-graph", "--limit", "1", "-T", "id")
	_, err := f.repo.sync(f.ctx, syncOptions{bookmark: bookmark}, io.Discard)
	assertRefusalGate(t, err, "outside-line")
	afterOp := jjTest(t, f.repo, "op", "log", "--no-graph", "--limit", "1", "-T", "id")
	if beforeOp != afterOp {
		t.Fatal("sync created a rewrite operation on refusal")
	}
	after := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	if before != after {
		t.Fatal("sync rewrote the owned bookmark on refusal")
	}
}

// TestSyncMixedStackRefusesBeforeRewrite pins a local grandparent, an
// origin-only parent that moved ahead on origin, and a local child. Sync must
// refuse foreign-published before it rewrites the child, so the op log and the
// child stay unchanged.
func TestSyncMixedStackRefusesBeforeRewrite(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	grand := f.new(t, "EX-1", "feat(rjc): local grandparent", "")
	f.commit(t, "grand.txt")
	jjTest(t, f.repo, "bookmark", "move", grand, "--to", "@-")
	parent := f.new(t, "EX-2", "feat(rjc): origin-only parent", grand)
	f.commit(t, "parent.txt")
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", parent)
	child := f.new(t, "EX-3", "feat(rjc): local child", parent)
	f.commit(t, "child.txt")
	jjTest(t, f.repo, "bookmark", "move", child, "--to", "@-")
	jjTest(t, f.repo, "bookmark", "forget", parent)
	other := cloneFixtureRepo(t, f.repo, "other-mixed-stack")
	jjTest(t, other, "bookmark", "track", quoteRev(parent+"@origin"))
	jjTest(t, other, "new", quoteRev(parent+"@origin"))
	writeTestFile(t, filepath.Join(other.root, "moved.txt"), "moved on origin\n")
	jjTest(t, other, "commit", "-m", "fix: parent moved on origin")
	jjTest(t, other, "bookmark", "set", parent, "-r", "@-")
	jjTest(t, other, "git", "push", "--bookmark", parent)
	jjTest(t, f.repo, "git", "fetch", "--remote", "origin")
	jjTest(t, f.repo, "new", quoteRev(child))
	before := opLogID(t, f)
	tipBefore := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(child), "-T", "commit_id")
	_, err := f.repo.sync(f.ctx, syncOptions{bookmark: child}, io.Discard)
	assertRefusalGate(t, err, "foreign-published")
	if after := opLogID(t, f); before != after {
		t.Fatalf("refusal changed op log: before=%s after=%s", before, after)
	}
	if tipAfter := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(child), "-T", "commit_id"); tipBefore != tipAfter {
		t.Fatalf("refusal moved child from %s to %s", tipBefore, tipAfter)
	}
}
