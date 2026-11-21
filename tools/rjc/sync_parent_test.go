package main

import (
	"io"
	"path/filepath"
	"slices"
	"testing"
)

func TestSyncSquashMergedParent(t *testing.T) {
	t.Parallel()
	f, parent, parentTip, child := parentFixture(t)
	f.prs[parent] = 1
	f.merged = map[string]bool{parent: true}
	jjTest(t, f.repo, "new", mainOrigin)
	f.commit(t, "parent.txt")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	deleteParentBranch(t, f, parent)
	plan, err := f.repo.planSync(f.ctx, child)
	if err != nil || len(plan.Stale) != 0 {
		t.Fatalf("landed parent stale = %v, error %v", plan.Stale, err)
	}
	jjTest(t, f.repo, "new", quoteRev(child))
	_, err = f.repo.line(f.ctx, child, lineOptions{})
	assertRefusalGate(t, err, "parent-boundary")
	_, err = f.repo.sync(f.ctx, syncOptions{bookmark: child}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := jjTest(t, f.repo, "log", "--no-graph", "-r", "::"+quoteRev(parentTip)+" & main@origin.."+quoteRev(child), "-T", "commit_id"); got != "" {
		t.Fatalf("parent remained: %s", got)
	}
	if state := savedParent(t, f, child); state.Bookmark != parent || !state.Landed {
		t.Fatalf("lost saved landed parent: %+v", state)
	}
	f.merged[parent] = false
	landed, _, err := f.repo.parentHasLanded(f.ctx, savedParent(t, f, child))
	if err != nil || !landed {
		t.Fatalf("saved landing was not retained: %v %v", landed, err)
	}
}

func TestSyncLiveRewrittenForeignParent(t *testing.T) {
	t.Parallel()
	f, parent, old, child := parentFixture(t)
	jjTest(t, f.repo, "new", mainOrigin)
	f.commit(t, "foreign-new.txt")
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-", "--allow-backwards")
	jjTest(t, f.repo, "git", "push", "--bookmark", parent)
	newTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(parent), "-T", "commit_id")
	jjTest(t, f.repo, "new", quoteRev(child))
	plan, err := f.repo.planSync(f.ctx, child)
	if err != nil || len(plan.Stale) != 0 {
		t.Fatalf("foreign parent stale = %v, error %v", plan.Stale, err)
	}
	_, err = f.repo.line(f.ctx, child, lineOptions{})
	assertRefusalGate(t, err, "parent-boundary")
	_, err = f.repo.sync(f.ctx, syncOptions{bookmark: child}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if newTip == old {
		t.Fatal("fixture did not move parent")
	}
	got := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(parent)+" & ::"+quoteRev(child), "-T", "commit_id")
	if got != newTip {
		t.Fatal("child not restacked onto parent")
	}
}

func TestSyncOwnedStack(t *testing.T) {
	t.Parallel()
	for _, published := range []bool{false, true} {
		t.Run(map[bool]string{false: "unpublished", true: "published"}[published], func(t *testing.T) {
			t.Parallel()
			f := parallelFixtureRepo(t)
			parent := f.new(t, "EX-1", "feat(rjc): add parent", "")
			f.commit(t, "parent.txt")
			jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
			if published {
				jjTest(t, f.repo, "git", "push", "--bookmark", parent)
			}
			child := f.new(t, "EX-2", "feat(rjc): add child", parent)
			f.commit(t, "child.txt")
			jjTest(t, f.repo, "bookmark", "move", child, "--to", "@-")
			before := map[string]string{}
			changes := map[string]string{}
			for _, name := range []string{parent, child} {
				before[name] = jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(name), "-T", "commit_id")
				changes[name] = jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(name), "-T", "change_id")
			}
			jjTest(t, f.repo, "new", mainOrigin)
			f.commit(t, "upstream.txt")
			jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
			jjTest(t, f.repo, "git", "push", "--bookmark", "main")
			jjTest(t, f.repo, "new", quoteRev(child))
			results, err := f.repo.sync(f.ctx, syncOptions{bookmark: child}, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if len(results) != 2 {
				t.Fatalf("results: %v", results)
			}
			for i, name := range []string{parent, child} {
				tip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(name), "-T", "commit_id")
				id := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(name), "-T", "change_id")
				if results[i].Bookmark != name || results[i].Tip != tip || tip == before[name] || id != changes[name] {
					t.Fatalf("unexpected moved segment %s: %+v, tip %s, change %s", name, results[i], tip, id)
				}
			}
		})
	}
}

func TestSyncRefusesForeignPublishedDescendantBeforeRewrite(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	parent := f.new(t, "EX-1", "feat(rjc): parent", "")
	f.commit(t, "parent.txt")
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
	foreign := "other-lane/published"
	jjTest(t, f.repo, "new", quoteRev(parent))
	f.commit(t, "foreign.txt")
	jjTest(t, f.repo, "bookmark", "create", foreign, "-r", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", foreign)
	jjTest(t, f.repo, "new", quoteRev(parent))
	before := jjTest(t, f.repo, "op", "log", "--no-graph", "--limit", "1", "-T", "id")
	_, err := f.repo.sync(f.ctx, syncOptions{bookmark: parent}, io.Discard)
	assertRefusalGate(t, err, "divergent-line")
	if got := jjTest(t, f.repo, "op", "log", "--no-graph", "--limit", "1", "-T", "id"); got != before {
		t.Fatal("refusal rewrote repository")
	}
}

func TestSyncRefusesMovedForeignParentBeforeRewrite(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	grandparent := f.new(t, "EX-1", "feat(rjc): owned grandparent", "")
	f.commit(t, "grandparent.txt")
	jjTest(t, f.repo, "bookmark", "move", grandparent, "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", grandparent)
	parent := f.new(t, "EX-2", "feat(rjc): parent to become foreign", grandparent)
	f.commit(t, "parent.txt")
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
	foreign := "other-lane/ex-2-foreign-parent"
	jjTest(t, f.repo, "bookmark", "rename", parent, foreign)
	jjTest(t, f.repo, "git", "push", "--bookmark", foreign)
	child := f.new(t, "EX-3", "feat(rjc): owned child of foreign parent", foreign)
	f.commit(t, "child.txt")
	jjTest(t, f.repo, "bookmark", "move", child, "--to", "@-")
	other := cloneFixtureRepo(t, f.repo, "other-clone")
	jjTest(t, other, "bookmark", "track", quoteRev(foreign+"@origin"))
	jjTest(t, other, "new", quoteRev(foreign+"@origin"))
	writeTestFile(t, filepath.Join(other.root, "foreign-moved.txt"), "moved on origin\n")
	jjTest(t, other, "commit", "-m", "fix: foreign parent moved on origin")
	jjTest(t, other, "bookmark", "set", foreign, "-r", "@-")
	jjTest(t, other, "git", "push", "--bookmark", foreign)
	jjTest(t, f.repo, "git", "fetch", "--remote", "origin")
	jjTest(t, f.repo, "new", quoteRev(child))
	childBefore := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(child), "-T", "commit_id")
	before := jjTest(t, f.repo, "op", "log", "--no-graph", "--limit", "1", "-T", "id")
	_, err := f.repo.sync(f.ctx, syncOptions{bookmark: child}, io.Discard)
	assertRefusalGate(t, err, "foreign-bookmark")
	if got := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(child), "-T", "commit_id"); got != childBefore {
		t.Fatalf("refusal rewrote child: %s, want %s", got, childBefore)
	}
	if got := jjTest(t, f.repo, "op", "log", "--no-graph", "--limit", "1", "-T", "id"); got != before {
		t.Fatal("refusal changed the op log")
	}
}

func TestSyncRefusesForeignBookmarkSharingOwnedTip(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	parent := f.new(t, "EX-1", "feat(rjc): parent", "")
	f.commit(t, "parent.txt")
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
	foreign := "other-lane/shared"
	jjTest(t, f.repo, "bookmark", "create", foreign, "-r", quoteRev(parent))
	jjTest(t, f.repo, "git", "push", "--bookmark", parent, "--bookmark", foreign)
	jjTest(t, f.repo, "new", quoteRev(parent))
	before := jjTest(t, f.repo, "op", "log", "--no-graph", "--limit", "1", "-T", "id")
	_, err := f.repo.sync(f.ctx, syncOptions{bookmark: parent}, io.Discard)
	assertRefusalGate(t, err, "divergent-line")
	if got := jjTest(t, f.repo, "op", "log", "--no-graph", "--limit", "1", "-T", "id"); got != before {
		t.Fatal("refusal rewrote repository")
	}
}

func TestSyncLocallyAmendedPublishedBookmark(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): parent", "")
	f.commit(t, "published.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", bookmark)
	jjTest(t, f.repo, "describe", "-r", quoteRev(bookmark), "-m", "feat(rjc): amended")
	jjTest(t, f.repo, "new", quoteRev(bookmark))
	results, err := f.repo.sync(f.ctx, syncOptions{bookmark: bookmark}, io.Discard)
	if err != nil || len(results) != 1 || results[0].Bookmark != bookmark {
		t.Fatalf("sync results=%v err=%v", results, err)
	}
}

func TestSyncOwnedParentDiverged(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	parent := f.new(t, "EX-1", "feat(rjc): add parent", "")
	f.commit(t, "parent.txt")
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", parent)
	child := f.new(t, "EX-2", "feat(rjc): add child", parent)
	f.commit(t, "child.txt")
	jjTest(t, f.repo, "bookmark", "move", child, "--to", "@-")
	jjTest(t, f.repo, "new", quoteRev(parent))
	f.commit(t, "additional.txt")
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
	jjTest(t, f.repo, "new", quoteRev(child))
	_, err := f.repo.sync(f.ctx, syncOptions{bookmark: child}, io.Discard)
	assertRefusalGate(t, err, "parent-diverged")
}

func TestSyncRewrittenOwnedParent(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	parent := f.new(t, "EX-1", "feat(rjc): add parent", "")
	f.commit(t, "parent.txt")
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", parent)
	old := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(parent), "-T", "commit_id")
	clone := filepath.Join(t.TempDir(), "clone")
	jjTest(t, f.repo, "git", "clone", filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(f.repo.root)))), "remote.git"), clone)
	child := f.new(t, "EX-2", "feat(rjc): add child", parent)
	f.commit(t, "child.txt")
	jjTest(t, f.repo, "bookmark", "move", child, "--to", "@-")
	other := *f.repo
	other.root = clone
	jjTest(t, &other, "git", "fetch", "--remote", "origin")
	jjTest(t, &other, "bookmark", "track", parent+"@origin")
	jjTest(t, &other, "new", quoteRev(parent))
	writeTestFile(t, filepath.Join(other.root, "rewrite.txt"), "rewritten parent\n")
	f.commit(t, "child-second.txt")
	jjTest(t, f.repo, "bookmark", "move", child, "--to", "@-")
	jjTest(t, &other, "squash")
	jjTest(t, f.repo, "git", "push", "--bookmark", child)
	jjTest(t, &other, "git", "push", "--bookmark", parent)
	jjTest(t, f.repo, "git", "fetch", "--remote", "origin")
	jjTest(t, f.repo, "rebase", "--ignore-immutable", "-s", quoteRev(child)+"-", "-d", old)
	foreign := "other-lane/foreign-descendant"
	jjTest(t, f.repo, "bookmark", "create", foreign, "-r", quoteRev(old))
	jjTest(t, f.repo, "new", quoteRev(foreign))
	f.commit(t, "foreign-descendant.txt")
	jjTest(t, f.repo, "bookmark", "move", foreign, "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", foreign)
	plan, err := f.repo.planSync(f.ctx, child)
	if err != nil || len(plan.Stale) != 1 || plan.Stale[0] != old {
		t.Fatalf("rewritten owned parent stale = %v, want %s, error %v", plan.Stale, old, err)
	}
	jjTest(t, f.repo, "new", quoteRev(child))
	beforeParent := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(parent), "-T", "commit_id")
	beforeChild := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(child), "-T", "commit_id")
	beforeOp := jjTest(t, f.repo, "op", "log", "--no-graph", "--limit", "1", "-T", "id")
	_, err = f.repo.sync(f.ctx, syncOptions{bookmark: child}, io.Discard)
	assertRefusalGate(t, err, "divergent-line")
	if got := jjTest(t, f.repo, "op", "log", "--no-graph", "--limit", "1", "-T", "id"); got != beforeOp {
		t.Fatal("refusal rewrote repository")
	}
	if got := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(parent), "-T", "commit_id"); got != beforeParent {
		t.Fatal("parent moved")
	}
	if got := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(child), "-T", "commit_id"); got != beforeChild {
		t.Fatal("child moved")
	}
}

func TestSyncPublishedTwoCommitChildOfRewrittenParent(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	parent := f.new(t, "EX-1", "feat(rjc): add parent", "")
	f.commit(t, "parent.txt")
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", parent)
	old := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(parent), "-T", "commit_id")
	clone := filepath.Join(t.TempDir(), "clone")
	jjTest(t, f.repo, "git", "clone", filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(f.repo.root)))), "remote.git"), clone)
	child := f.new(t, "EX-2", "feat(rjc): add child", parent)
	f.commit(t, "child.txt")
	f.commit(t, "child-second.txt")
	jjTest(t, f.repo, "bookmark", "move", child, "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", child)
	other := *f.repo
	other.root = clone
	jjTest(t, &other, "bookmark", "track", parent+"@origin")
	jjTest(t, &other, "new", quoteRev(parent))
	writeTestFile(t, filepath.Join(other.root, "rewrite.txt"), "rewritten parent\n")
	jjTest(t, &other, "squash")
	jjTest(t, &other, "git", "push", "--bookmark", parent)
	jjTest(t, f.repo, "git", "fetch", "--remote", "origin")
	jjTest(t, f.repo, "rebase", "--ignore-immutable", "-s", quoteRev(child)+"--", "-d", old)
	jjTest(t, f.repo, "new", quoteRev(child))
	beforeOp := jjTest(t, f.repo, "op", "log", "--no-graph", "--limit", "1", "-T", "id")
	_, err := f.repo.sync(f.ctx, syncOptions{bookmark: child}, io.Discard)
	assertRefusalGate(t, err, "divergent-line")
	if got := jjTest(t, f.repo, "op", "log", "--no-graph", "--limit", "1", "-T", "id"); got != beforeOp {
		t.Fatal("refusal rewrote repository")
	}
}

func TestSyncTrackingForkMergedParent(t *testing.T) {
	t.Parallel()
	f, parent, _, child := parentFixture(t)
	jjTest(t, f.repo, "bookmark", "create", "rigel-release", "-r", mainOrigin)
	jjTest(t, f.repo, "git", "push", "--bookmark", "rigel-release")
	f.repo.profile.Posture = forkTracking
	f.repo.profile.Trunk = "rigel-release"
	f.prs[parent] = 1
	f.merged = map[string]bool{parent: true}
	jjTest(t, f.repo, "new", "rigel-release@origin")
	f.commit(t, "parent.txt")
	jjTest(t, f.repo, "bookmark", "move", "rigel-release", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "rigel-release")
	deleteParentBranch(t, f, parent)
	jjTest(t, f.repo, "new", quoteRev(child))
	_, err := f.repo.sync(f.ctx, syncOptions{bookmark: child}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := jjTest(t, f.repo, "log", "--no-graph", "-r", "rigel-release@origin & ::"+quoteRev(child), "-T", "commit_id"); got == "" {
		t.Fatal("child not on release trunk")
	}
}

func TestSyncReconcilesInsertedParent(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bottom := f.new(t, "EX-1", "feat(rjc): bottom work", "")
	f.commit(t, "bottom.txt")
	jjTest(t, f.repo, "bookmark", "move", bottom, "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", bottom)
	top := f.new(t, "EX-3", "feat(rjc): top work", bottom)
	f.commit(t, "top.txt")
	jjTest(t, f.repo, "bookmark", "move", top, "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", top)
	middle := f.new(t, "EX-2", "feat(rjc): middle work inserted later", bottom)
	f.commit(t, "middle.txt")
	jjTest(t, f.repo, "bookmark", "move", middle, "--to", "@-")
	jjTest(t, f.repo, "rebase", "--ignore-immutable", "-b", quoteRev(top), "-d", quoteRev(middle))
	jjTest(t, f.repo, "git", "push", "--bookmark", middle)
	jjTest(t, f.repo, "git", "push", "--bookmark", top)
	jjTest(t, f.repo, "new", quoteRev(top))
	plan, err := f.repo.planSync(f.ctx, top)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, segment := range plan.Segments {
		order = append(order, segment.Bookmark)
	}
	if want := []string{bottom, middle, top}; !slices.Equal(order, want) {
		t.Fatalf("planned segments = %v, want %v", order, want)
	}
	results, err := f.repo.sync(f.ctx, syncOptions{bookmark: top}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("results = %v", results)
	}
	state := savedParent(t, f, top)
	if state.Bookmark != middle {
		t.Fatalf("saved parent = %s, want %s", state.Bookmark, middle)
	}
	if got := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(middle), "-T", "commit_id"); state.Tip != got {
		t.Fatalf("saved parent tip = %s, want middle's tip %s", state.Tip, got)
	}
}

func TestSyncRefusesAmbiguousInsertedParent(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bottom := f.new(t, "EX-1", "feat(rjc): bottom work", "")
	f.commit(t, "bottom.txt")
	jjTest(t, f.repo, "bookmark", "move", bottom, "--to", "@-")
	top := f.new(t, "EX-4", "feat(rjc): top work", bottom)
	f.commit(t, "top.txt")
	jjTest(t, f.repo, "bookmark", "move", top, "--to", "@-")
	first := f.new(t, "EX-2", "feat(rjc): first inserted work", bottom)
	f.commit(t, "first.txt")
	jjTest(t, f.repo, "bookmark", "move", first, "--to", "@-")
	second := f.new(t, "EX-3", "feat(rjc): second inserted work", bottom)
	f.commit(t, "second.txt")
	jjTest(t, f.repo, "bookmark", "move", second, "--to", "@-")
	jjTest(t, f.repo, "rebase", "-b", quoteRev(second), "-d", quoteRev(first))
	jjTest(t, f.repo, "rebase", "-b", quoteRev(top), "-d", quoteRev(second))
	jjTest(t, f.repo, "new", quoteRev(top))
	_, err := f.repo.planSync(f.ctx, top)
	assertRefusalGate(t, err, "ambiguous-parent")
	_, err = f.repo.sync(f.ctx, syncOptions{bookmark: top}, io.Discard)
	assertRefusalGate(t, err, "ambiguous-parent")
	if state := savedParent(t, f, top); state.Bookmark != bottom {
		t.Fatalf("refused sync rewrote the saved parent to %s", state.Bookmark)
	}
}

func TestSyncReconcilesInsertedParentAfterSavedParentLands(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bottom := f.new(t, "EX-1", "feat(rjc): bottom work", "")
	f.commit(t, "bottom.txt")
	jjTest(t, f.repo, "bookmark", "move", bottom, "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", bottom)
	bottomTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bottom), "-T", "commit_id")
	top := f.new(t, "EX-3", "feat(rjc): top work", bottom)
	f.commit(t, "top.txt")
	jjTest(t, f.repo, "bookmark", "move", top, "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", top)
	middle := f.new(t, "EX-2", "feat(rjc): middle work inserted later", bottom)
	f.commit(t, "middle.txt")
	jjTest(t, f.repo, "bookmark", "move", middle, "--to", "@-")
	jjTest(t, f.repo, "rebase", "--ignore-immutable", "-b", quoteRev(top), "-d", quoteRev(middle))
	jjTest(t, f.repo, "git", "push", "--bookmark", middle)
	jjTest(t, f.repo, "git", "push", "--bookmark", top)
	f.prs[bottom] = 1
	f.merged = map[string]bool{bottom: true}
	f.mergedHeads = map[string]string{bottom: bottomTip}
	jjTest(t, f.repo, "new", mainOrigin)
	f.commit(t, "parent.txt")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	jjTest(t, f.repo, "new", quoteRev(top))
	plan, err := f.repo.planSync(f.ctx, top)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, segment := range plan.Segments {
		order = append(order, segment.Bookmark)
	}
	if want := []string{middle, top}; !slices.Equal(order, want) {
		t.Fatalf("planned segments = %v, want %v", order, want)
	}
	if _, err := f.repo.sync(f.ctx, syncOptions{bookmark: top}, io.Discard); err != nil {
		t.Fatal(err)
	}
	state := savedParent(t, f, top)
	if state.Bookmark != middle {
		t.Fatalf("saved parent = %s, want %s", state.Bookmark, middle)
	}
	if got := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(middle), "-T", "commit_id"); state.Tip != got {
		t.Fatalf("saved parent tip = %s, want middle's tip %s", state.Tip, got)
	}
}

func TestSyncConflictAfterInsertedParentKeepsSavedParent(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bottom := f.new(t, "EX-1", "feat(rjc): bottom work", "")
	f.commit(t, "bottom.txt")
	jjTest(t, f.repo, "bookmark", "move", bottom, "--to", "@-")
	top := f.new(t, "EX-3", "feat(rjc): top work", bottom)
	f.commit(t, "top.txt")
	jjTest(t, f.repo, "bookmark", "move", top, "--to", "@-")
	middle := f.new(t, "EX-2", "feat(rjc): middle work inserted later", bottom)
	f.commit(t, "middle.txt")
	jjTest(t, f.repo, "bookmark", "move", middle, "--to", "@-")
	jjTest(t, f.repo, "rebase", "-b", quoteRev(top), "-d", quoteRev(middle))
	jjTest(t, f.repo, "new", mainOrigin)
	writeTestFile(t, filepath.Join(f.repo.root, "middle.txt"), "trunk\n")
	jjTest(t, f.repo, "commit", "-m", "fix: trunk")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	jjTest(t, f.repo, "new", quoteRev(top))
	before := savedParent(t, f, top)
	_, err := f.repo.sync(f.ctx, syncOptions{bookmark: top}, io.Discard)
	assertRefusalGate(t, err, "conflict")
	if after := savedParent(t, f, top); after.Bookmark != before.Bookmark || after.Tip != before.Tip {
		t.Fatalf("refused sync changed the saved parent from %s@%s to %s@%s", before.Bookmark, before.Tip, after.Bookmark, after.Tip)
	}
}

func TestSyncRefusesInsertedParentWithoutSavedParent(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bottom := f.new(t, "EX-1", "feat(rjc): bottom work", "")
	f.commit(t, "bottom.txt")
	jjTest(t, f.repo, "bookmark", "move", bottom, "--to", "@-")
	top := f.new(t, "EX-3", "feat(rjc): top work", bottom)
	f.commit(t, "top.txt")
	jjTest(t, f.repo, "bookmark", "move", top, "--to", "@-")
	middle := f.new(t, "EX-2", "feat(rjc): middle work inserted later", bottom)
	f.commit(t, "middle.txt")
	jjTest(t, f.repo, "bookmark", "move", middle, "--to", "@-")
	if err := f.repo.clearParent(middle); err != nil {
		t.Fatal(err)
	}
	jjTest(t, f.repo, "rebase", "-b", quoteRev(top), "-d", quoteRev(middle))
	jjTest(t, f.repo, "new", quoteRev(top))
	_, err := f.repo.planSync(f.ctx, top)
	assertRefusalGate(t, err, "unlinked-parent")
	_, err = f.repo.sync(f.ctx, syncOptions{bookmark: top}, io.Discard)
	assertRefusalGate(t, err, "unlinked-parent")
	if state := savedParent(t, f, top); state.Bookmark != bottom {
		t.Fatalf("refused sync rewrote the saved parent to %s", state.Bookmark)
	}
}
