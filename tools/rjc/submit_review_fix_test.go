package main

import (
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestSubmitReviewFixAdvancesParentAndRestacksChild(t *testing.T) {
	t.Parallel()
	f, parent, child := publishedReviewFixStack(t)
	oldParentTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(parent), "-T", "commit_id")
	oldChildTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(child), "-T", "commit_id")

	jjTest(t, f.repo, "new", quoteRev(parent))
	f.commit(t, "parent-review-fix.txt")
	fixTip := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: parent, title: "feat(rjc): add review-fix parent", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}

	newParentTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(parent), "-T", "commit_id")
	newChildTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(child), "-T", "commit_id")
	if newParentTip != fixTip || newParentTip == oldParentTip {
		t.Fatalf("parent tip = %s, want review fix %s (was %s)", newParentTip, fixTip, oldParentTip)
	}
	if newChildTip == oldChildTip {
		t.Fatalf("child tip did not move off the old parent: %s", newChildTip)
	}
	if got := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(parent)+" & ::"+quoteRev(child), "-T", "commit_id"); got != newParentTip {
		t.Fatalf("child does not descend from updated parent %s: %s", newParentTip, got)
	}
	if state := savedParent(t, f, child); state.Bookmark != parent || state.Tip != newParentTip {
		t.Fatalf("saved child parent = %+v, want %s at %s", state, parent, newParentTip)
	}
	diff := jjTest(t, f.repo, "diff", "--from", quoteRev(parent+"@origin"), "--to", quoteRev(child+"@origin"), "--summary")
	if strings.Contains(diff, "parent-review-fix.txt") || !strings.Contains(diff, "child.txt") {
		t.Fatalf("child PR diff = %s", diff)
	}
	if strings.Join(f.pushes, ",") != parent+","+child+","+parent+","+child {
		t.Fatalf("push order = %v", f.pushes)
	}
	if got := f.metadata[f.prs[parent]].HeadSHA; got != newParentTip {
		t.Fatalf("parent PR head = %s, want %s", got, newParentTip)
	}
	if got := f.metadata[f.prs[child]].HeadSHA; got != newChildTip {
		t.Fatalf("child PR head = %s, want %s", got, newChildTip)
	}
}

func TestSubmitChildKeepsInsertedParentFixOutOfChildPR(t *testing.T) {
	t.Parallel()
	f, parent, child := publishedReviewFixStack(t)
	oldParentTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(parent), "-T", "commit_id")
	childRoot := jjTest(t, f.repo, "log", "--no-graph", "-r", "roots("+quoteRev(parent)+".."+quoteRev(child)+")", "-T", "commit_id")

	jjTest(t, f.repo, "new", quoteRev(parent))
	f.commit(t, "inserted-parent-review-fix.txt")
	fixTip := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	jjTest(t, f.repo, "rebase", "--ignore-immutable", "-s", quoteRev(childRoot), "-d", quoteRev(fixTip))
	jjTest(t, f.repo, "new", quoteRev(child))

	if err := f.repo.submit(f.ctx, submitOptions{bookmark: child, title: "feat(rjc): add review-fix child", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(parent), "-T", "commit_id"); got != fixTip {
		t.Fatalf("parent tip = %s, want %s (was %s)", got, fixTip, oldParentTip)
	}
	diff := jjTest(t, f.repo, "diff", "--from", quoteRev(parent+"@origin"), "--to", quoteRev(child+"@origin"), "--summary")
	if strings.Contains(diff, "inserted-parent-review-fix.txt") || !strings.Contains(diff, "child.txt") {
		t.Fatalf("child PR diff = %s", diff)
	}
	if state := savedParent(t, f, child); state.Tip != fixTip {
		t.Fatalf("saved child parent = %+v, want %s", state, fixTip)
	}
	if strings.Join(f.pushes, ",") != parent+","+child+","+parent+","+child {
		t.Fatalf("push order = %v", f.pushes)
	}
}

func TestSubmitInsertedFixRefusesClosedForkParent(t *testing.T) {
	t.Parallel()
	f, parent, child := publishedReviewFixStack(t)
	root := jjTest(t, f.repo, "log", "--no-graph", "-r", "roots("+quoteRev(parent)+".."+quoteRev(child)+")", "-T", "commit_id")
	jjTest(t, f.repo, "new", quoteRev(parent))
	f.commit(t, "inserted-parent-review-fix.txt")
	fix := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	jjTest(t, f.repo, "rebase", "--ignore-immutable", "-s", quoteRev(root), "-d", quoteRev(fix))
	jjTest(t, f.repo, "new", quoteRev(child))
	f.repo.profile.Posture = forkTracking
	delete(f.prs, parent)
	before := opLogID(t, f)
	err := f.repo.submit(f.ctx, submitOptions{bookmark: child, title: "feat(rjc): add review-fix child", bodyFile: f.bodyFile}, f, io.Discard)
	assertRefusalGate(t, err, "closed-fork-parent")
	if opLogID(t, f) != before || len(f.pushes) != 2 {
		t.Fatalf("refusal mutated history or pushed: %v", f.pushes)
	}
}

func TestSubmitReviewFixKeepsParentStateAfterChildPushFailure(t *testing.T) {
	t.Parallel()
	f, parent, child := publishedReviewFixStack(t)
	old := savedParent(t, f, child).Tip
	jjTest(t, f.repo, "new", quoteRev(parent))
	f.commit(t, "parent-review-fix.txt")
	fix := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	f.afterPush = func() { f.pushError = errors.New("child push failed") }
	err := f.repo.submit(f.ctx, submitOptions{bookmark: parent, title: "feat(rjc): add review-fix parent", bodyFile: f.bodyFile}, f, io.Discard)
	if err == nil || savedParent(t, f, child).Tip != old || strings.Join(f.pushes, ",") != parent+","+child+","+parent+","+child || f.metadata[f.prs[parent]].HeadSHA != fix {
		t.Fatalf("child failure changed saved parent: err=%v state=%+v pushes=%v", err, savedParent(t, f, child), f.pushes)
	}
	f.pushError, f.afterPush = nil, nil
	jjTest(t, f.repo, "new", quoteRev(child))
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: child, title: "feat(rjc): add review-fix child", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if savedParent(t, f, child).Tip != fix {
		t.Fatal("retry did not update saved parent")
	}
	diff := jjTest(t, f.repo, "diff", "--from", quoteRev(parent+"@origin"), "--to", quoteRev(child+"@origin"), "--summary")
	if strings.Contains(diff, "parent-review-fix.txt") {
		t.Fatalf("child PR diff includes fix: %s", diff)
	}
}

func TestSubmitReviewFixReorderedChildRefuses(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	parent := f.new(t, "EX-1", "feat(rjc): add review-fix parent", "")
	f.commit(t, "parent.txt")
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
	child := f.new(t, "EX-2", "feat(rjc): add review-fix child", parent)
	f.commit(t, "child-one.txt")
	first := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(child), "-T", "change_id")
	f.commit(t, "child-two.txt")
	second := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "change_id")
	f.commit(t, "child-three.txt")
	jjTest(t, f.repo, "bookmark", "move", child, "--to", "@-")
	if err := f.repo.saveCandidate(f.ctx, parent, "feat(rjc): add review-fix parent", f.bodyFile, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: child, title: "feat(rjc): add review-fix child", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	jjTest(t, f.repo, "rebase", "--ignore-immutable", "-r", quoteRev(second), "-B", quoteRev(first))
	jjTest(t, f.repo, "new", quoteRev(child))
	before := opLogID(t, f)
	err := f.repo.submit(f.ctx, submitOptions{bookmark: child, title: "feat(rjc): add review-fix child", bodyFile: f.bodyFile}, f, io.Discard)
	assertRefusalGate(t, err, "parent-boundary")
	if opLogID(t, f) != before || len(f.pushes) != 2 {
		t.Fatalf("refusal mutated history or pushed: %v", f.pushes)
	}
}

func TestSubmitReviewFixLeavesUnpublishedChildLocal(t *testing.T) {
	t.Parallel()
	f, parent, child := publishedReviewFixStack(t)
	unpublished := f.new(t, "EX-3", "feat(rjc): add unpublished child", child)
	f.commit(t, "unpublished.txt")
	jjTest(t, f.repo, "bookmark", "move", unpublished, "--to", "@-")
	jjTest(t, f.repo, "new", quoteRev(parent))
	f.commit(t, "parent-review-fix.txt")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: parent, title: "feat(rjc): add review-fix parent", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.prs[unpublished] != 0 || strings.Join(f.pushes, ",") != parent+","+child+","+parent+","+child {
		t.Fatalf("unpublished child pushed: %v", f.pushes)
	}
	if got := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(parent)+" & ::"+quoteRev(unpublished), "-T", "commit_id"); got == "" {
		t.Fatal("unpublished child not restacked")
	}
}

func TestSubmitReviewFixRefusesUnsafeBranches(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"foreign-on-fix", "merge", "forked-owned"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			f, parent, _ := publishedReviewFixStack(t)
			if kind == "forked-owned" {
				other := f.new(t, "EX-3", "feat(rjc): add forked child", parent)
				f.commit(t, "fork.txt")
				jjTest(t, f.repo, "bookmark", "move", other, "--to", "@-")
			}
			jjTest(t, f.repo, "new", quoteRev(parent))
			if kind == "merge" {
				jjTest(t, f.repo, "new", mainOrigin)
				f.commit(t, "other.txt")
				other := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
				jjTest(t, f.repo, "new", quoteRev(parent), quoteRev(other))
			}
			f.commit(t, "parent-review-fix.txt")
			fix := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
			if kind == "foreign-on-fix" {
				f.commit(t, "foreign.txt")
				jjTest(t, f.repo, "bookmark", "create", "other-lane/ex-4-foreign", "-r", "@-")
				jjTest(t, f.repo, "new", quoteRev(fix))
			}
			before := opLogID(t, f)
			err := f.repo.submit(f.ctx, submitOptions{bookmark: parent, title: "feat(rjc): add review-fix parent", bodyFile: f.bodyFile}, f, io.Discard)
			gate := "foreign-descendant"
			if kind == "merge" {
				gate = "nonlinear"
			}
			assertRefusalGate(t, err, gate)
			if opLogID(t, f) != before || len(f.pushes) != 2 {
				t.Fatalf("refusal mutated history or pushed: %v", f.pushes)
			}
		})
	}
}

func TestSubmitReviewFixRestacksPublishedGrandchild(t *testing.T) {
	t.Parallel()
	f, parent, child := publishedReviewFixStack(t)
	grandchild := f.new(t, "EX-3", "feat(rjc): add review-fix grandchild", child)
	f.commit(t, "grandchild.txt")
	jjTest(t, f.repo, "bookmark", "move", grandchild, "--to", "@-")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: grandchild, title: "feat(rjc): add review-fix grandchild", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	f.pushes = nil
	jjTest(t, f.repo, "new", quoteRev(parent))
	f.commit(t, "parent-review-fix.txt")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: parent, title: "feat(rjc): add review-fix parent", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.pushes, ",") != parent+","+child+","+grandchild {
		t.Fatalf("push order: %v", f.pushes)
	}
	if savedParent(t, f, grandchild).Tip != jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(child), "-T", "commit_id") {
		t.Fatal("grandchild parent was not updated")
	}
}

func TestSubmitReviewFixStopsOnRestackConflict(t *testing.T) {
	t.Parallel()
	f, parent, child := publishedReviewFixStack(t)
	parentTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(parent), "-T", "commit_id")
	childTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(child), "-T", "commit_id")
	jjTest(t, f.repo, "new", quoteRev(parent))
	writeTestFile(t, filepath.Join(f.repo.root, "child.txt"), "parent fix\n")
	jjTest(t, f.repo, "commit", "-m", "fix: conflicting review fix")
	err := f.repo.submit(f.ctx, submitOptions{bookmark: parent, title: "feat(rjc): add review-fix parent", bodyFile: f.bodyFile}, f, io.Discard)
	assertRefusalGate(t, err, "conflict")
	var detail *failure
	if !errors.As(err, &detail) || !strings.Contains(detail.reason, "child.txt") || strings.Contains(detail.fix, "op restore") || len(f.pushes) != 2 {
		t.Fatalf("conflict did not stop with file details and no op-restore hint: %v, pushes=%v", err, f.pushes)
	}
	for _, want := range []string{
		"jj bookmark set " + parent + " -r " + parentTip + " --allow-backwards",
		"jj bookmark set " + child + " -r " + childTip + " --allow-backwards",
	} {
		if !strings.Contains(detail.fix, want) {
			t.Fatalf("conflict refusal must name recovery %q, got fix:\n%s", want, detail.fix)
		}
	}
}

func TestApplyReviewFixAbandonsEveryRewrittenChildCommit(t *testing.T) {
	t.Parallel()
	f, parent, child := publishedReviewFixStack(t)
	jjTest(t, f.repo, "new", quoteRev(child))
	writeTestFile(t, filepath.Join(f.repo.root, "child.txt"), "child lower\n")
	jjTest(t, f.repo, "commit", "-m", "fix: child lower")
	writeTestFile(t, filepath.Join(f.repo.root, "child.txt"), "child tip\n")
	jjTest(t, f.repo, "commit", "-m", "fix: child tip")
	jjTest(t, f.repo, "bookmark", "move", child, "--to", "@-")
	childTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(child), "-T", "commit_id")
	childSegment, err := f.repo.revisions(f.ctx, quoteRev(parent)+".."+quoteRev(child))
	if err != nil {
		t.Fatal(err)
	}
	if len(childSegment) < 2 {
		t.Fatalf("child segment has %d commits, want at least two", len(childSegment))
	}
	jjTest(t, f.repo, "new", quoteRev(parent))
	writeTestFile(t, filepath.Join(f.repo.root, "child.txt"), "parent fix\n")
	jjTest(t, f.repo, "commit", "-m", "fix: conflicting review fix")
	fix := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	parentTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(parent), "-T", "commit_id")
	line := []Change{
		{Bookmark: parent, Tip: parentTip, Base: mainOrigin, Owned: true},
		{Bookmark: child, Tip: childTip, Base: parent, Owned: true},
	}
	childRoot := jjTest(t, f.repo, "log", "--no-graph", "-r", "roots("+quoteRev(parent)+".."+quoteRev(child)+")", "-T", "commit_id")
	plan := &reviewFixPlan{parent: parent, fix: fix, childRoot: childRoot, line: line}
	_, err = f.repo.applyReviewFix(f.ctx, plan)
	assertRefusalGate(t, err, "conflict")
	detail, ok := errors.AsType[*failure](err)
	if !ok {
		t.Fatalf("missing conflict recovery: %v", err)
	}
	runSyncRecovery(t, f, detail.fix, syncPlan{Segments: []syncSegment{{Bookmark: child, Commits: childSegment}}})
	changes := make([]string, 0, len(childSegment))
	for _, commit := range childSegment {
		changes = append(changes, "change_id("+quoteRev(jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(commit), "-T", "change_id"))+")")
	}
	visible := strings.TrimSpace(jjTest(t, f.repo, "log", "--no-graph", "-r", "("+strings.Join(changes, " | ")+") ~ "+syncSet(childSegment), "-T", `commit_id ++ "\n"`))
	if visible != "" {
		t.Fatalf("rewritten child copies stay visible after review-fix recovery:\n%s", visible)
	}
}

func TestSubmitReviewFixRefusesForeignDescendant(t *testing.T) {
	t.Parallel()
	f, parent, child := publishedReviewFixStack(t)
	jjTest(t, f.repo, "new", quoteRev(child))
	f.commit(t, "foreign-descendant.txt")
	jjTest(t, f.repo, "bookmark", "create", "other-lane/ex-3-foreign", "-r", "@-")
	jjTest(t, f.repo, "new", quoteRev(parent))
	f.commit(t, "parent-review-fix.txt")
	before := opLogID(t, f)
	err := f.repo.submit(f.ctx, submitOptions{bookmark: parent, title: "feat(rjc): add review-fix parent", bodyFile: f.bodyFile}, f, io.Discard)
	assertRefusalGate(t, err, "foreign-descendant")
	if got := opLogID(t, f); got != before || len(f.pushes) != 2 {
		t.Fatalf("refusal changed history or pushed: op=%s, pushes=%v", got, f.pushes)
	}
}

func TestSubmitReviewFixRefusesUnownedPublishedDescendant(t *testing.T) {
	t.Parallel()
	f, parent, child := publishedReviewFixStack(t)
	jjTest(t, f.repo, "new", quoteRev(child))
	f.commit(t, "foreign-review.txt")
	foreign := "other-lane/ex-4-foreign-review"
	jjTest(t, f.repo, "bookmark", "create", foreign, "-r", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", foreign)
	jjTest(t, f.repo, "bookmark", "forget", foreign)
	jjTest(t, f.repo, "new", quoteRev(parent))
	f.commit(t, "parent-review-fix.txt")
	before := opLogID(t, f)
	err := f.repo.submit(f.ctx, submitOptions{bookmark: parent, title: "feat(rjc): add review-fix parent", bodyFile: f.bodyFile}, f, io.Discard)
	assertRefusalGate(t, err, "foreign-published")
	if opLogID(t, f) != before || len(f.pushes) != 2 {
		t.Fatalf("refusal mutated history or pushed: %v", f.pushes)
	}
}

func publishedReviewFixStack(t *testing.T) (*fixture, string, string) {
	t.Helper()
	f := parallelFixtureRepo(t)
	parent := f.new(t, "EX-1", "feat(rjc): add review-fix parent", "")
	f.commit(t, "parent.txt")
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
	child := f.new(t, "EX-2", "feat(rjc): add review-fix child", parent)
	f.commit(t, "child.txt")
	jjTest(t, f.repo, "bookmark", "move", child, "--to", "@-")
	if err := f.repo.saveCandidate(f.ctx, parent, "feat(rjc): add review-fix parent", f.bodyFile, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: child, title: "feat(rjc): add review-fix child", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	return f, parent, child
}

func TestSubmitLowerBookmarkRefusesOwnedChildLineAbove(t *testing.T) {
	t.Parallel()
	t.Run("first-commit", func(t *testing.T) {
		t.Parallel()
		s := lowerWithChildLine(t)
		assertLowerRefusesChildLineFix(t, s, s.first)
	})
	t.Run("later-commit", func(t *testing.T) {
		t.Parallel()
		s := lowerWithChildLine(t)
		assertLowerRefusesChildLineFix(t, s, s.second)
	})
}

// lowerChildLine is a lower bookmark with a two-commit owned child line above it.
type lowerChildLine struct {
	f                      *fixture
	lower, lowerTip, upper string
	first, second          string
}

func lowerWithChildLine(t *testing.T) lowerChildLine {
	t.Helper()
	f := parallelFixtureRepo(t)
	lower := f.new(t, "EX-1", "feat(rjc): add review-fix parent", "")
	f.commit(t, "parent.txt")
	jjTest(t, f.repo, "bookmark", "move", lower, "--to", "@-")
	s := lowerChildLine{f: f, lower: lower}
	s.lowerTip = jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(lower), "-T", "commit_id")
	s.upper = f.new(t, "EX-2", "feat(rjc): add stacked child", lower)
	s.first = jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	f.commit(t, "child-root.txt")
	s.second = jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	f.commit(t, "child.txt")
	jjTest(t, f.repo, "bookmark", "move", s.upper, "--to", "@-")
	jjTest(t, f.repo, "new", mainOrigin)
	f.commit(t, "main-advance.txt")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	return s
}

func assertLowerRefusesChildLineFix(t *testing.T, s lowerChildLine, fix string) {
	t.Helper()
	jjTest(t, s.f.repo, "new", quoteRev(fix))
	before := opLogID(t, s.f)
	err := s.f.repo.submit(s.f.ctx, submitOptions{bookmark: s.lower, title: "feat(rjc): add review-fix parent", bodyFile: s.f.bodyFile}, s.f, io.Discard)
	assertRefusalGate(t, err, "fix-on-child-line")
	detail, ok := errors.AsType[*failure](err)
	if !ok || !strings.Contains(detail.fix, "rjc meta "+s.lower+" ") || !strings.Contains(detail.fix, "rjc submit "+s.upper) {
		t.Fatalf("refusal hint must name a working path, got: %v", err)
	}
	if opLogID(t, s.f) != before || len(s.f.pushes) != 0 {
		t.Fatalf("refusal mutated history or pushed: %v", s.f.pushes)
	}
	if got := jjTest(t, s.f.repo, "log", "--no-graph", "-r", quoteRev(s.lower), "-T", "commit_id"); got != s.lowerTip {
		t.Fatalf("lower bookmark moved to %s, want its own tip %s", got, s.lowerTip)
	}
}
