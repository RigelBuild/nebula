package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFetchMainRefusesOtherDefault(t *testing.T) {
	f := fixtureRepo(t)
	if err := f.repo.fetchMain(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.commit(t, "trunk.txt")
	jjTest(t, f.repo, "bookmark", "create", "trunk", "-r", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "trunk")
	remote := filepath.Join(os.Getenv("HOME"), "remote.git")
	if _, err := f.repo.command(f.ctx, nil, nil, "git", "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/trunk"); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.fetchMain(f.ctx); err == nil || !strings.Contains(err.Error(), "origin HEAD differs from the configured trunk") {
		t.Fatalf("expected default branch refusal, got %v", err)
	}
}

func TestNewUsesSelectedRemoteParent(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	parent := f.new(t, "EX-1", "feat(rjc): add remote parent", "")
	f.commit(t, "parent.txt")
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", parent)
	parentTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(parent), "-T", "commit_id")
	child := f.new(t, "EX-2", "feat(rjc): use fixed remote parent", parent)
	childParent := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(child)+"-", "-T", "commit_id")
	if childParent != parentTip {
		t.Fatalf("child base = %s, want %s", childParent, parentTip)
	}
	description := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(child), "-T", "description")
	if !strings.HasPrefix(description, "feat(rjc): use fixed remote parent (EX-2)\n") || !strings.Contains(description, "Co-authored-by: Example Author <author@example.com>") {
		t.Fatalf("new root description: %s", description)
	}
	if tip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(parent), "-T", "commit_id"); tip != parentTip {
		t.Fatal("new rewrote its parent")
	}
}

func TestNewAtWorkBeforeMainAdvance(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	f.commit(t, "feature.txt")
	work := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "change_id")
	jjTest(t, f.repo, "new", mainOrigin)
	f.commit(t, "unrelated.txt")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	bookmark, err := f.repo.newChange(f.ctx, newOptions{issue: "EX-1", title: "feat(rjc): adopt work", at: work})
	if err != nil {
		t.Fatal(err)
	}
	got := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "change_id")
	if got != work {
		t.Fatalf("bookmark change = %s, want %s", got, work)
	}
	base := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark)+"-", "-T", "commit_id")
	main := jjTest(t, f.repo, "log", "--no-graph", "-r", mainOrigin, "-T", "commit_id")
	if base != main {
		t.Fatalf("base = %s, want %s", base, main)
	}
}

func TestNewAtMainAncestorRefuses(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	ancestor := jjTest(t, f.repo, "log", "--no-graph", "-r", mainOrigin, "-T", "commit_id")
	f.commit(t, "main-advance.txt")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	_, err := f.repo.newChange(f.ctx, newOptions{issue: "EX-1", title: "feat(rjc): adopt work", at: ancestor})
	assertRefusalGate(t, err, "wrong-base")
}

func TestNewAtRefusesDivergentCopyOutsideLine(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, diverge string
		submit        bool
	}{{"tip", "@-", false}, {"lower", "@--", false}, {"submit-adoption", "@-", true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := parallelFixtureRepo(t)
			f.commit(t, "lower.txt")
			f.commit(t, "tip.txt")
			target := jjTest(t, f.repo, "log", "--no-graph", "-r", tc.diverge, "-T", "commit_id")
			old := opLogID(t, f)
			jjTest(t, f.repo, "describe", "-r", target, "-m", "fix: local rewrite")
			// A concurrent operation from the older op rewrites the same change
			// onto main, outside the line.
			jjTest(t, f.repo, "--at-op", old, "--ignore-working-copy", "rebase", "-r", target, "-d", mainOrigin)
			if got := jjTest(t, f.repo, "log", "--no-graph", "-r", "divergent()", "-T", `commit_id ++ "\n"`); len(strings.Fields(got)) < 2 {
				t.Fatalf("fixture did not make the change divergent: %q", got)
			}
			at := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
			before := opLogID(t, f)
			var err error
			if tc.submit {
				// submit without a bookmark adopts @- through newChange(at: "@-").
				err = f.repo.submit(f.ctx, submitOptions{issue: "EX-1", title: "feat(rjc): adopt work", bodyFile: f.bodyFile}, f, io.Discard)
			} else {
				_, err = f.repo.newChange(f.ctx, newOptions{issue: "EX-1", title: "feat(rjc): adopt work", at: at})
			}
			assertRefusalGate(t, err, "divergent-line")
			if after := opLogID(t, f); before != after {
				t.Fatalf("refusal changed op log: before=%s after=%s", before, after)
			}
		})
	}
}

func TestNewAtConflictRefuses(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	writeTestFile(t, filepath.Join(f.repo.root, "feature.txt"), "work\n")
	jjTest(t, f.repo, "commit", "-m", "fix: work")
	work := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "change_id")
	jjTest(t, f.repo, "new", mainOrigin)
	writeTestFile(t, filepath.Join(f.repo.root, "feature.txt"), "main\n")
	jjTest(t, f.repo, "commit", "-m", "fix: main")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	_, err := f.repo.newChange(f.ctx, newOptions{issue: "EX-1", title: "feat(rjc): adopt work", at: work})
	assertRefusalGate(t, err, "conflict")
	if !strings.Contains(err.Error(), "feature.txt") {
		t.Fatalf("conflict missing file: %v", err)
	}
}

func TestNewAtConflictResolveRetry(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	writeTestFile(t, filepath.Join(f.repo.root, "feature.txt"), "work\n")
	jjTest(t, f.repo, "commit", "-m", "fix: work")
	work := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "change_id")
	jjTest(t, f.repo, "new", mainOrigin)
	writeTestFile(t, filepath.Join(f.repo.root, "feature.txt"), "main\n")
	jjTest(t, f.repo, "commit", "-m", "fix: main")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	opt := newOptions{issue: "EX-1", title: "feat(rjc): adopt work", at: work}
	_, err := f.repo.newChange(f.ctx, opt)
	assertRefusalGate(t, err, "conflict")
	jjTest(t, f.repo, "edit", quoteRev(work))
	writeTestFile(t, filepath.Join(f.repo.root, "feature.txt"), "resolved\n")
	jjTest(t, f.repo, "new")
	if _, err := f.repo.newChange(f.ctx, opt); err != nil {
		t.Fatal(err)
	}
}

func TestCheckLinearOwnershipRefusesBookmarkOnLineOutsideSelection(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): protect selected line", "")
	f.commit(t, "interior.txt")
	jjTest(t, f.repo, "bookmark", "create", "other-lane/ex-2-interior", "-r", "@-")
	f.commit(t, "tip.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	line := []Change{{Bookmark: bookmark, Tip: tip, Base: mainOrigin, Owned: true}}
	assertRefusalGate(t, f.repo.checkLinearOwnership(f.ctx, line, false), "outside-line")
}

func TestLineRefusesSiblingOwnedBookmarks(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	first := f.new(t, "EX-1", "feat(rjc): create first branch", "")
	f.commit(t, "first.txt")
	jjTest(t, f.repo, "bookmark", "move", first, "--to", "@-")
	jjTest(t, f.repo, "new", mainOrigin)
	f.commit(t, "second.txt")
	second := "test-lane/ex-2-second-branch"
	jjTest(t, f.repo, "bookmark", "create", second, "-r", "@-")
	jjTest(t, f.repo, "new", first, second)
	f.commit(t, "merge.txt")
	jjTest(t, f.repo, "bookmark", "move", second, "--to", "@-")
	_, err := f.repo.line(f.ctx, second, lineOptions{})
	assertRefusalGate(t, err, "nonlinear")
	if !strings.Contains(err.Error(), "one root") {
		t.Fatalf("expected one-root refusal, got %v", err)
	}
}

func TestCheckLinearOwnershipRefusesMultipleRootsWithoutMerge(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	f.commit(t, "first.txt")
	first := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	jjTest(t, f.repo, "new", mainOrigin)
	f.commit(t, "second.txt")
	second := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	root, err := f.repo.singleRoot(f.ctx, "roots(("+quoteRev(first)+" | "+quoteRev(second)+") ~ ::"+mainOrigin+")")
	if err == nil || !strings.Contains(err.Error(), "one root") {
		t.Fatalf("root = %q, expected one-root refusal, got %v", root, err)
	}
}

func TestLineRefusesForeignBookmarkAheadOfOrigin(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): protect line", "")
	f.commit(t, "foreign-base.txt")
	foreign := "other-lane/ex-2-parent"
	jjTest(t, f.repo, "bookmark", "create", foreign, "-r", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", foreign)
	f.commit(t, "foreign-ahead.txt")
	jjTest(t, f.repo, "bookmark", "move", foreign, "--to", "@-")
	f.commit(t, "owned.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	_, err := f.repo.line(f.ctx, bookmark, lineOptions{})
	assertRefusalGate(t, err, "outside-line")
}

func TestLineRefusesMainInsideOwnedLine(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): protect local main", "")
	f.commit(t, "main-ahead.txt")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	f.commit(t, "owned.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	_, err := f.repo.line(f.ctx, bookmark, lineOptions{})
	assertRefusalGate(t, err, "outside-line")
}

func TestLineRefusesUnpublishedForeignBookmark(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): protect unpublished parent", "")
	f.commit(t, "foreign.txt")
	jjTest(t, f.repo, "bookmark", "create", "other-lane/ex-2-local", "-r", "@-")
	f.commit(t, "owned.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	_, err := f.repo.line(f.ctx, bookmark, lineOptions{})
	assertRefusalGate(t, err, "foreign-bookmark")
}

func TestForeignDescendantsAllowsWorkingCopyWithWork(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): allow working copy", "")
	f.commit(t, "interior.txt")
	interior := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	f.commit(t, "tip.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	jjTest(t, f.repo, "new", quoteRev(interior), "-m", "test: working copy")
	writeTestFile(t, filepath.Join(f.repo.root, "working.txt"), "work\n")
	line := []Change{{Bookmark: bookmark, Tip: tip, Base: mainOrigin, Owned: true}}
	if err := f.repo.checkLinearOwnership(f.ctx, line, false); err != nil {
		t.Fatalf("working copy refused: %v", err)
	}
}

func TestForeignDescendantsAllowsEmptyUndescribedSibling(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): allow empty sibling", "")
	f.commit(t, "interior.txt")
	interior := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	f.commit(t, "tip.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	jjTest(t, f.repo, "new", quoteRev(interior))
	jjTest(t, f.repo, "new")
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	line := []Change{{Bookmark: bookmark, Tip: tip, Base: mainOrigin, Owned: true}}
	if err := f.repo.checkLinearOwnership(f.ctx, line, false); err != nil {
		t.Fatalf("empty undescribed sibling refused: %v", err)
	}
}

func TestRebaseRefusesForeignBookmarkedChild(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): protect child", "")
	f.commit(t, "owned.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	line, err := f.repo.line(f.ctx, bookmark, lineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f.commit(t, "foreign.txt")
	foreign := "other-lane/ex-2-foreign-child"
	jjTest(t, f.repo, "bookmark", "create", foreign, "-r", "@-")
	before := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(foreign), "-T", "commit_id")
	err = f.repo.rebaseOwned(f.ctx, line, false, nil)
	assertRefusalGate(t, err, "foreign-descendant")
	after := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(foreign), "-T", "commit_id")
	if before != after {
		t.Fatalf("foreign child changed from %s to %s", before, after)
	}
}

func TestRebaseRefusesForeignInteriorSibling(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): protect sibling", "")
	f.commit(t, "interior.txt")
	interior := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	f.commit(t, "tip.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	jjTest(t, f.repo, "new", quoteRev(interior))
	f.commit(t, "sibling.txt")
	foreign := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "change_id")
	before := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(foreign), "-T", "commit_id")
	line := []Change{{Bookmark: bookmark, Tip: tip, Base: mainOrigin, Owned: true}}
	assertRefusalGate(t, f.repo.rebaseOwned(f.ctx, line, false, nil), "foreign-descendant")
	after := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(foreign), "-T", "commit_id")
	if before != after {
		t.Fatalf("foreign sibling changed from %s to %s", before, after)
	}
}

func TestRebaseRefusesPublishedInteriorSibling(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): protect published sibling", "")
	f.commit(t, "interior.txt")
	interior := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	f.commit(t, "tip.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", bookmark)
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	jjTest(t, f.repo, "new", quoteRev(interior))
	f.commit(t, "sibling.txt")
	foreign := "other-lane/ex-2-published-sibling"
	jjTest(t, f.repo, "bookmark", "create", foreign, "-r", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", foreign)
	jjTest(t, f.repo, "bookmark", "forget", foreign)
	before := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(foreign+"@origin"), "-T", "commit_id")
	line := []Change{{Bookmark: bookmark, Tip: tip, Base: mainOrigin, Owned: true}}
	assertRefusalGate(t, f.repo.rebaseOwned(f.ctx, line, false, nil), "foreign-published")
	after := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(foreign+"@origin"), "-T", "commit_id")
	if before != after {
		t.Fatalf("published sibling changed from %s to %s", before, after)
	}
}

func TestRebaseRefusesPublishedWorkingCopy(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): scan working copy", "")
	f.commit(t, "owned.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	foreign := "other-lane/ex-2-working-copy"
	writeTestFile(t, filepath.Join(f.repo.root, "published.txt"), "published\n")
	jjTest(t, f.repo, "describe", "-m", "test: publish working copy")
	jjTest(t, f.repo, "bookmark", "create", foreign, "-r", "@")
	jjTest(t, f.repo, "git", "push", "--bookmark", foreign)
	jjTest(t, f.repo, "bookmark", "forget", foreign)
	before := jjTest(t, f.repo, "log", "--no-graph", "-r", "@", "-T", "commit_id")
	line := []Change{{Bookmark: bookmark, Tip: tip, Base: mainOrigin, Owned: true}}
	assertRefusalGate(t, f.repo.rebaseOwned(f.ctx, line, false, nil), "foreign-published")
	after := jjTest(t, f.repo, "log", "--no-graph", "-r", "@", "-T", "commit_id")
	if before != after {
		t.Fatalf("published working copy changed from %s to %s", before, after)
	}
}

func TestRebaseRefusesPublishedWorkingCopyInRange(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): protect published working copy", "")
	jjTest(t, f.repo, "new", mainOrigin)
	writeTestFile(t, filepath.Join(f.repo.root, "published.txt"), "published\n")
	jjTest(t, f.repo, "describe", "-m", "test: published work")
	foreign := "other-lane/ex-2-published-working-copy"
	jjTest(t, f.repo, "bookmark", "create", foreign, "-r", "@")
	jjTest(t, f.repo, "git", "push", "--bookmark", foreign)
	jjTest(t, f.repo, "bookmark", "forget", foreign)
	jjTest(t, f.repo, "new")
	f.commit(t, "owned.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--allow-backwards", "--to", "@-")
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	jjTest(t, f.repo, "edit", "--ignore-immutable", quoteRev(foreign+"@origin"))
	before := jjTest(t, f.repo, "log", "--no-graph", "-r", "@", "-T", "commit_id")
	line := []Change{{Bookmark: bookmark, Tip: tip, Base: mainOrigin, Owned: true}}
	assertRefusalGate(t, f.repo.rebaseOwned(f.ctx, line, false, nil), "foreign-published")
	after := jjTest(t, f.repo, "log", "--no-graph", "-r", "@", "-T", "commit_id")
	if before != after {
		t.Fatalf("published working copy changed from %s to %s", before, after)
	}
}

func TestRebaseRefusesForeignPublishedSource(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): protect published work", "")
	f.commit(t, "published.txt")
	foreign := "other-lane/ex-2-published"
	jjTest(t, f.repo, "bookmark", "create", foreign, "-r", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", foreign)
	jjTest(t, f.repo, "bookmark", "forget", foreign)
	f.commit(t, "owned.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	line := []Change{{Bookmark: bookmark, Tip: tip, Base: mainOrigin, Owned: true}}
	assertRefusalGate(t, f.repo.rebaseOwned(f.ctx, line, false, nil), "foreign-published")
}

func TestNewAtRebasesOntoSelectedParent(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	parent := f.new(t, "EX-1", "feat(rjc): add parent", "")
	f.commit(t, "parent.txt")
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", parent)
	parentOld := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(parent), "-T", "commit_id")
	jjTest(t, f.repo, "new", quoteRev(parent))
	f.commit(t, "child.txt")
	work := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "change_id")
	jjTest(t, f.repo, "new", quoteRev(parent))
	f.commit(t, "parent-next.txt")
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", parent)
	parentTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(parent), "-T", "commit_id")
	if parentTip == parentOld {
		t.Fatal("parent did not advance")
	}
	child, err := f.repo.newChange(f.ctx, newOptions{issue: "EX-2", title: "feat(rjc): adopt child", on: parent, at: work})
	if err != nil {
		t.Fatal(err)
	}
	base := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(child)+"-", "-T", "commit_id")
	if base != parentTip {
		t.Fatalf("child base = %s, want parent %s", base, parentTip)
	}
}

func TestNewAtRefusesForeignDescendant(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	f.commit(t, "feature.txt")
	work := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "change_id")
	jjTest(t, f.repo, "bookmark", "create", "other-lane/ex-2-foreign")
	jjTest(t, f.repo, "new", mainOrigin)
	f.commit(t, "main.txt")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	_, err := f.repo.newChange(f.ctx, newOptions{issue: "EX-1", title: "feat(rjc): adopt work", at: work})
	assertRefusalGate(t, err, "foreign-descendant")
}

func TestRebaseLineAfterUnrelatedMainChange(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): rebase on main", "")
	f.commit(t, "feature.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	line, err := f.repo.line(f.ctx, bookmark, lineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	jjTest(t, f.repo, "new", mainOrigin)
	f.commit(t, "unrelated.txt")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	if _, err := f.repo.rebaseLine(f.ctx, line, false); err != nil {
		t.Fatal(err)
	}
	main := jjTest(t, f.repo, "log", "--no-graph", "-r", mainOrigin, "-T", "commit_id")
	if got := jjTest(t, f.repo, "log", "--no-graph", "-r", "roots("+mainOrigin+".."+quoteRev(bookmark)+")-", "-T", "commit_id"); got != main {
		t.Fatalf("rebased root parent = %s, want %s", got, main)
	}
}

func TestPreflightIdentityAndBoundary(t *testing.T) {
	f := fixtureRepo(t)
	t.Chdir(f.repo.root)
	jjTest(t, f.repo, "git", "remote", "set-url", "origin", "https://github.com/example/app.git")
	r, err := preflight(f.ctx, loadExampleConfig(t), "", io.Discard)
	if err != nil || r.codename != "test-lane" {
		t.Fatalf("preflight = %+v, %v", r, err)
	}
	jjTest(t, f.repo, "git", "remote", "set-url", "origin", "https://github.com/foreign/app.git")
	_, err = preflight(f.ctx, loadExampleConfig(t), "", io.Discard)
	assertRefusalGate(t, err, "repository")
	jjTest(t, f.repo, "git", "remote", "set-url", "origin", "https://github.com/example/mirror-test.git")
	_, err = preflight(f.ctx, loadExampleConfig(t), "", io.Discard)
	assertRefusalGate(t, err, "repository")
	var detail *failure
	if !errors.As(err, &detail) || !strings.Contains(detail.reason, "repository does not accept PRs (mirror target)") || strings.Contains(detail.reason+detail.fix, "landing") {
		t.Fatalf("mirror refusal = %v", err)
	}
	jjTest(t, f.repo, "git", "remote", "set-url", "origin", "https://github.com/example/app.git")
	jjTest(t, f.repo, "config", "set", "--workspace", "user.email", "wrong@example.test")
	_, err = preflight(f.ctx, loadExampleConfig(t), "", io.Discard)
	assertRefusalGate(t, err, "identity")
	outside := &repository{root: filepath.Join(os.Getenv("HOME"), "untrusted"), log: io.Discard}
	if err := os.MkdirAll(outside.root, 0o700); err != nil {
		t.Fatal(err)
	}
	jjTest(t, outside, "git", "init", "--colocate")
	t.Chdir(outside.root)
	_, err = preflight(f.ctx, loadExampleConfig(t), "test-lane", io.Discard)
	assertRefusalGate(t, err, "write-boundary")
}

func TestRefusesEditsAndOutsideLineBookmarks(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): protect bookmark ownership", "")
	writeTestFile(t, filepath.Join(f.repo.root, "dirty.txt"), "uncommitted\n")
	assertRefusalGate(t, f.repo.clean(f.ctx), "uncommitted-edits")
	jjTest(t, f.repo, "commit", "-m", "fix: save dirty work")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	jjTest(t, f.repo, "new", bookmark, "-m", "feat: sibling branch")
	jjTest(t, f.repo, "bookmark", "create", "another-lane/ex-2-foreign-descendant")
	jjTest(t, f.repo, "new")
	_, err := f.repo.line(f.ctx, bookmark, lineOptions{})
	assertRefusalGate(t, err, "foreign-descendant")
}

func assertRefusalGate(t *testing.T, err error, gate string) {
	t.Helper()
	detail, ok := errors.AsType[*failure](err)
	if !ok || detail.gate != gate {
		t.Fatalf("error = %v, want gate %s", err, gate)
	}
}
