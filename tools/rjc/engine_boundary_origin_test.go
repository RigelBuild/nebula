package main

import (
	"io"
	"path/filepath"
	"testing"
)

// originOnlyParent publishes an owned parent, stacks an owned child on it and
// deletes the parent's local bookmark, so the parent exists only as
// parent@origin.
func originOnlyParent(t *testing.T, f *fixture) (parent, child string) {
	t.Helper()
	parent = f.new(t, "EX-1", "feat(rjc): publish parent for origin-only stack", "")
	f.commit(t, "parent.txt")
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", parent)
	child = f.new(t, "EX-2", "feat(rjc): stack on the current origin tip", parent)
	f.commit(t, "child.txt")
	jjTest(t, f.repo, "bookmark", "move", child, "--to", "@-")
	jjTest(t, f.repo, "bookmark", "delete", parent)
	return parent, child
}

// TestParentResolvesToOriginWhenLocalBookmarkGone covers the sync plan and
// engine boundary for an owned parent whose only copy is parent@origin.
func TestParentResolvesToOriginWhenLocalBookmarkGone(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	parent, child := originOnlyParent(t, f)
	plan, err := f.repo.planSync(f.ctx, child)
	if err != nil {
		t.Fatalf("planSync with origin-only parent: %v", err)
	}
	if len(plan.Segments) != 1 || plan.Segments[0].Bookmark != child || plan.Destination != parent+"@origin" {
		t.Fatalf("plan = %+v, want one child segment based on %s@origin", plan, parent)
	}
	change := Change{Bookmark: child, Tip: jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(child), "-T", "commit_id"), Base: parent, Owned: true}
	if err := f.repo.checkEngineSelection(f.ctx, change, true); err != nil {
		t.Fatalf("engine selection with origin-only parent: %v", err)
	}
}

func TestSubmitDryRunOriginOnlyParent(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	parent, child := originOnlyParent(t, f)
	jjTest(t, f.repo, "new", quoteRev(child))
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: child, title: "feat(rjc): stack on the current origin tip", bodyFile: f.bodyFile, dryRun: true}, f, io.Discard); err != nil {
		t.Fatalf("submit --dry-run with origin-only parent: %v", err)
	}
	line, err := f.repo.line(f.ctx, child, lineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(line) != 1 || line[0].Base != parent+"@origin" {
		t.Fatalf("line = %+v, want child based on %s@origin", line, parent)
	}
}

func TestSyncOriginOnlyParent(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	parent, child := originOnlyParent(t, f)
	jjTest(t, f.repo, "new", quoteRev(child))
	results, err := f.repo.sync(f.ctx, syncOptions{bookmark: child}, io.Discard)
	if err != nil {
		t.Fatalf("sync with origin-only parent: %v", err)
	}
	if len(results) != 1 || results[0].Bookmark != child || results[0].Base != parent+"@origin" {
		t.Fatalf("results = %+v, want child based on %s@origin", results, parent)
	}
}

// TestSyncConflictedLocalParentRefused checks that a conflicted local parent
// bookmark is refused, not silently replaced by its origin copy.
func TestSyncConflictedLocalParentRefused(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	parent := f.new(t, "EX-1", "feat(rjc): publish parent for conflicted stack", "")
	f.commit(t, "parent.txt")
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", parent)
	child := f.new(t, "EX-2", "feat(rjc): stack on a conflicted parent", parent)
	f.commit(t, "child.txt")
	jjTest(t, f.repo, "bookmark", "move", child, "--to", "@-")
	op := jjTest(t, f.repo, "op", "log", "--no-graph", "--limit", "1", "-T", "id")
	jjTest(t, f.repo, "bookmark", "set", parent, "-r", "root()", "--allow-backwards")
	jjTest(t, f.repo, "--at-op", op, "bookmark", "set", parent, "-r", quoteRev(child), "--allow-backwards")
	jjTest(t, f.repo, "new", quoteRev(child))
	_, err := f.repo.sync(f.ctx, syncOptions{bookmark: child}, io.Discard)
	assertRefusalGate(t, err, "ambiguous-revision")
}

// TestMixedStackOriginOnlyParentRefused pins a grandparent that is local, a
// parent that exists only on origin, and a local child. The local grandparent
// sits beneath the origin-only parent, so submit and sync must both refuse with
// foreign-published instead of disagreeing about the child's base.
func TestMixedStackOriginOnlyParentRefused(t *testing.T) {
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
	jjTest(t, f.repo, "bookmark", "delete", parent)
	jjTest(t, f.repo, "new", quoteRev(child))
	if placed, err := f.repo.originOnlyBase(f.ctx, parent, false); err != nil || !placed {
		t.Fatalf("origin-only parent with no placed line above it: placed=%v err=%v", placed, err)
	}
	if carried, err := f.repo.originOnlyBase(f.ctx, parent, true); err != nil || carried {
		t.Fatalf("parent above a local grandparent must not be origin-based: got %v err=%v", carried, err)
	}
	line, err := f.repo.line(f.ctx, child, lineOptions{skipOwnership: true})
	if err != nil {
		t.Fatalf("line with local grandparent: %v", err)
	}
	if len(line) != 2 || line[1].Bookmark != child || line[1].Base != grand {
		t.Fatalf("line = %+v, want child based on local grandparent %s", line, grand)
	}
	err = f.repo.submit(f.ctx, submitOptions{bookmark: child, title: "feat(rjc): local child", bodyFile: f.bodyFile, dryRun: true}, f, io.Discard)
	assertRefusalGate(t, err, "foreign-published")
	_, err = f.repo.sync(f.ctx, syncOptions{bookmark: child}, io.Discard)
	assertRefusalGate(t, err, "foreign-published")
}

// TestSyncRefusesDivergentOriginOnlyParent covers a parent that was published,
// amended locally, and left with no local bookmark. The child carries the
// amended copy beside origin's copy, so sync must refuse as divergent-line.
func TestSyncRefusesDivergentOriginOnlyParent(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	parent, child := originOnlyParent(t, f)
	jjTest(t, f.repo, "describe", "--ignore-immutable", "-r", quoteRev(parent+"@origin"), "-m", "feat(rjc): parent amended locally")
	jjTest(t, f.repo, "new", quoteRev(child))
	before := opLogID(t, f)
	_, err := f.repo.sync(f.ctx, syncOptions{bookmark: child}, io.Discard)
	assertRefusalGate(t, err, "divergent-line")
	if after := opLogID(t, f); before != after {
		t.Fatalf("refusal changed op log: before=%s after=%s", before, after)
	}
}

// TestSyncRefusesDeletedParentMovedOnOrigin covers a tracked parent deleted
// locally, then moved on origin. The fetch conflicts the local bookmark; sync
// must refuse with an rjc gate, not the raw jj conflict error.
func TestSyncRefusesDeletedParentMovedOnOrigin(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	parent := f.new(t, "EX-1", "feat(rjc): tracked parent", "")
	f.commit(t, "parent.txt")
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", parent)
	child := f.new(t, "EX-2", "feat(rjc): child of moved parent", parent)
	f.commit(t, "child.txt")
	jjTest(t, f.repo, "bookmark", "move", child, "--to", "@-")
	jjTest(t, f.repo, "bookmark", "delete", parent)
	other := cloneFixtureRepo(t, f.repo, "other-moved-parent")
	jjTest(t, other, "bookmark", "track", quoteRev(parent+"@origin"))
	jjTest(t, other, "new", quoteRev(parent+"@origin"))
	writeTestFile(t, filepath.Join(other.root, "moved.txt"), "moved on origin\n")
	jjTest(t, other, "commit", "-m", "fix: parent moved on origin")
	jjTest(t, other, "bookmark", "set", parent, "-r", "@-")
	jjTest(t, other, "git", "push", "--bookmark", parent)
	jjTest(t, f.repo, "git", "fetch", "--remote", "origin")
	jjTest(t, f.repo, "new", quoteRev(child))
	_, err := f.repo.sync(f.ctx, syncOptions{bookmark: child}, io.Discard)
	assertRefusalGate(t, err, "ambiguous-revision")
}

// TestSyncOwnedParentLandedAndDeleted covers an owned parent that squash-landed
// with its branch deleted on origin and locally. Sync must stop at the landed
// parent instead of refusing the missing parent@origin.
func TestSyncOwnedParentLandedAndDeleted(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	parent := f.new(t, "EX-1", "feat(rjc): owned parent that lands", "")
	f.commit(t, "parent.txt")
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", parent)
	child := f.new(t, "EX-2", "feat(rjc): child of landed owned parent", parent)
	f.commit(t, "child.txt")
	jjTest(t, f.repo, "bookmark", "move", child, "--to", "@-")
	f.prs[parent] = 1
	f.merged = map[string]bool{parent: true}
	f.mergedHeads = map[string]string{parent: jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(parent), "-T", "commit_id")}
	jjTest(t, f.repo, "new", mainOrigin)
	f.commit(t, "parent.txt")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	deleteParentBranch(t, f, parent)
	jjTest(t, f.repo, "new", quoteRev(child))
	if _, err := f.repo.sync(f.ctx, syncOptions{bookmark: child}, io.Discard); err != nil {
		t.Fatalf("sync with landed, deleted owned parent: %v", err)
	}
}

// TestSyncLandedOriginOnlyParentAboveStaleLocal covers two landed owned
// parents, grand and parent. The parent's local bookmark is gone, but grand's
// local bookmark remains below it. Sync must stop at the landed parent, not
// refuse grand's bookmark as a local owned base.
func TestSyncLandedOriginOnlyParentAboveStaleLocal(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	grand := f.new(t, "EX-1", "feat(rjc): grand that lands", "")
	f.commit(t, "grand.txt")
	jjTest(t, f.repo, "bookmark", "move", grand, "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", grand)
	parent := f.new(t, "EX-2", "feat(rjc): parent that lands", grand)
	f.commit(t, "parent.txt")
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", parent)
	child := f.new(t, "EX-3", "feat(rjc): child of landed parent", parent)
	f.commit(t, "child.txt")
	jjTest(t, f.repo, "bookmark", "move", child, "--to", "@-")
	f.prs[grand] = 1
	f.prs[parent] = 2
	f.merged = map[string]bool{grand: true, parent: true}
	f.mergedHeads = map[string]string{grand: jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(grand), "-T", "commit_id"), parent: jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(parent), "-T", "commit_id")}
	jjTest(t, f.repo, "new", mainOrigin)
	f.commit(t, "grand.txt")
	f.commit(t, "parent.txt")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	jjTest(t, f.repo, "bookmark", "delete", parent)
	jjTest(t, f.repo, "new", quoteRev(child))
	if _, err := f.repo.sync(f.ctx, syncOptions{bookmark: child}, io.Discard); err != nil {
		t.Fatalf("sync with landed origin-only parent above stale local grand: %v", err)
	}
}

// TestSyncConflictedTargetRefused checks that a conflicted target bookmark is
// refused as ambiguous, not left to fail with jj's raw conflict error.
func TestSyncConflictedTargetRefused(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	child := f.new(t, "EX-2", "feat(rjc): conflicted target", "")
	f.commit(t, "child.txt")
	jjTest(t, f.repo, "bookmark", "move", child, "--to", "@-")
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	op := jjTest(t, f.repo, "op", "log", "--no-graph", "--limit", "1", "-T", "id")
	jjTest(t, f.repo, "bookmark", "set", child, "-r", mainOrigin, "--allow-backwards")
	jjTest(t, f.repo, "--at-op", op, "bookmark", "set", child, "-r", "root()", "--allow-backwards")
	jjTest(t, f.repo, "new", quoteRev(tip))
	_, err := f.repo.sync(f.ctx, syncOptions{bookmark: child}, io.Discard)
	assertRefusalGate(t, err, "ambiguous-revision")
}
