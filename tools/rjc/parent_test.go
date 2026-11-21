package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func parentFixture(t *testing.T) (*fixture, string, string, string) {
	t.Helper()
	f := parallelFixtureRepo(t)
	parent := f.new(t, "EX-1", "feat(rjc): parent work to land", "")
	f.commit(t, "parent.txt")
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
	foreign := strings.Replace(parent, "test-lane/", "other-lane/", 1)
	jjTest(t, f.repo, "bookmark", "rename", parent, foreign)
	jjTest(t, f.repo, "git", "push", "--bookmark", foreign)
	parentTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(foreign), "-T", "commit_id")
	f.mergedHeads = map[string]string{foreign: parentTip}
	child := f.new(t, "EX-2", "feat(rjc): land the child independently", foreign)
	f.commit(t, "child.txt")
	jjTest(t, f.repo, "bookmark", "move", child, "--to", "@-")
	return f, foreign, parentTip, child
}

func TestLandedParentRelationshipRetires(t *testing.T) {
	t.Parallel()
	for _, deleted := range []bool{false, true} {
		t.Run(map[bool]string{false: "branch retained", true: "branch deleted"}[deleted], func(t *testing.T) {
			t.Parallel()
			f, parent, parentTip, child := parentFixture(t)
			f.prs[parent] = 1
			f.merged = map[string]bool{parent: true}
			landAndRestackChild(t, f, parent, parentTip, child, deleted)
			if err := f.repo.submit(f.ctx, submitOptions{bookmark: child, title: "feat(rjc): land the child independently", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
				t.Fatal(err)
			}
			if len(f.pushes) != 1 || f.pushes[0] != child {
				t.Fatalf("push set = %v", f.pushes)
			}
			if got := f.metadata[f.prs[child]].BaseRef; got != "main" {
				t.Fatalf("child PR base = %q", got)
			}
			path, err := (fileBaseline{root: f.repo.stateDir, keys: f.repo.keys}).path(child, ".parent.json")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("landed relationship was not retired: %v", err)
			}
		})
	}
}

func landAndRestackChild(t *testing.T, f *fixture, parent, parentTip, child string, deleted bool) {
	t.Helper()
	jjTest(t, f.repo, "new", mainOrigin)
	f.commit(t, "parent.txt")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	roots := jjTest(t, f.repo, "log", "--no-graph", "-r", "roots("+quoteRev(parentTip)+".."+quoteRev(child)+")", "-T", "commit_id")
	jjTest(t, f.repo, "rebase", "--ignore-immutable", "-s", roots, "-d", mainOrigin)
	if deleted {
		jjTest(t, f.repo, "bookmark", "delete", parent)
		jjTest(t, f.repo, "git", "push", "--bookmark", parent)
	}
	jjTest(t, f.repo, "new", quoteRev(child))
}

func TestPublishedChildRetargetsLandedParent(t *testing.T) {
	t.Parallel()
	f, parent, parentTip, child := parentFixture(t)
	f.prs[parent] = 1
	opt := submitOptions{bookmark: child, title: "feat(rjc): land the child independently", bodyFile: f.bodyFile}
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.metadata[f.prs[child]].BaseRef != parent {
		t.Fatal("initial child base was not the live parent")
	}
	f.pushes = nil
	f.merged = map[string]bool{parent: true}
	landAndRestackChild(t, f, parent, parentTip, child, false)
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(f.pushes) != 1 || f.pushes[0] != child || f.metadata[f.prs[child]].BaseRef != "main" {
		t.Fatalf("published child did not retarget: pushes=%v metadata=%v", f.pushes, f.metadata)
	}
}

func TestReachableParentRelationshipRetiresWithoutPR(t *testing.T) {
	t.Parallel()
	f, parent, parentTip, child := parentFixture(t)
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", parentTip)
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	jjTest(t, f.repo, "bookmark", "delete", parent)
	jjTest(t, f.repo, "git", "push", "--bookmark", parent)
	jjTest(t, f.repo, "new", quoteRev(child))
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: child, title: "feat(rjc): land the child independently", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(f.pushes) != 1 || f.pushes[0] != child || f.metadata[f.prs[child]].BaseRef != "main" {
		t.Fatalf("reachable parent submission: pushes=%v metadata=%v", f.pushes, f.metadata)
	}
}

func TestMergedParentWorkMustLeaveChildRange(t *testing.T) {
	t.Parallel()
	f, parent, _, child := parentFixture(t)
	f.prs[parent] = 1
	f.merged = map[string]bool{parent: true}
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: child, title: "feat(rjc): land the child independently", bodyFile: f.bodyFile}, f, io.Discard); err == nil || !strings.Contains(err.Error(), "parent-boundary") {
		t.Fatalf("old parent work was accepted: %v", err)
	}
	if len(f.pushes) != 0 {
		t.Fatalf("parent work reached engine: %v", f.pushes)
	}
}

func TestParentStateFailsClosed(t *testing.T) {
	t.Parallel()
	for _, data := range []string{"{", `{}`, `{"bookmark":"other-lane/ex-1-parent"}`, `{"tip":"abc"}`} {
		t.Run(data, func(t *testing.T) {
			t.Parallel()
			f := parallelFixtureRepo(t)
			bookmark := f.new(t, "EX-1", "feat(rjc): refuse corrupt parent state", "")
			path, err := (fileBaseline{root: f.repo.stateDir, keys: f.repo.keys}).path(bookmark, ".parent.json")
			if err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, path, data)
			err = f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: "feat(rjc): refuse corrupt parent state", bodyFile: f.bodyFile}, f, io.Discard)
			if err == nil || (!strings.Contains(err.Error(), "decode selected parent") && !strings.Contains(err.Error(), "invalid selected parent") && !strings.Contains(err.Error(), "full 40-hex")) {
				t.Fatalf("corrupt state refusal = %v", err)
			}
			if len(f.pushes) != 0 || len(f.prs) != 0 {
				t.Fatal("corrupt parent state reached engine")
			}
		})
	}
}

func savedParent(t *testing.T, f *fixture, bookmark string) parentRelationship {
	t.Helper()
	path, err := (fileBaseline{root: f.repo.stateDir, keys: f.repo.keys}).path(bookmark, ".parent.json")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var parent parentRelationship
	if err := json.Unmarshal(data, &parent); err != nil {
		t.Fatal(err)
	}
	return parent
}

func TestLiveParentCannotMoveBehindOldWork(t *testing.T) {
	t.Parallel()
	f, parent, _, child := parentFixture(t)
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", mainOrigin, "--allow-backwards")
	jjTest(t, f.repo, "git", "push", "--bookmark", parent)
	jjTest(t, f.repo, "new", quoteRev(child))
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: child, title: "feat(rjc): land the child independently", bodyFile: f.bodyFile}, f, io.Discard); err == nil || !strings.Contains(err.Error(), "parent-boundary") {
		t.Fatalf("retained old work was accepted: %v", err)
	}
	if len(f.pushes) != 0 {
		t.Fatalf("old parent work reached engine: %v", f.pushes)
	}
}

func TestLandedParentStateSurvivesFailedPush(t *testing.T) {
	t.Parallel()
	f, parent, parentTip, child := parentFixture(t)
	f.prs[parent] = 1
	f.merged = map[string]bool{parent: true}
	landAndRestackChild(t, f, parent, parentTip, child, false)
	opt := submitOptions{bookmark: child, title: "feat(rjc): land the child independently", bodyFile: f.bodyFile}
	f.pushError = errors.New("fixture failed push")
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); !errors.Is(err, f.pushError) {
		t.Fatalf("failed push = %v", err)
	}
	if savedParent(t, f, child).Bookmark != parent {
		t.Fatal("failed push lost the parent proof")
	}
	f.pushError = nil
	f.pushes = nil
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(f.pushes) != 1 || f.pushes[0] != child {
		t.Fatalf("retry push set = %v", f.pushes)
	}
}

func TestMergedParentInteriorWorkStillRefuses(t *testing.T) {
	t.Parallel()
	f, parent, parentTip, child := parentFixture(t)
	f.prs[parent] = 1
	f.merged = map[string]bool{parent: true}
	roots := jjTest(t, f.repo, "log", "--no-graph", "-r", "roots("+quoteRev(parentTip)+".."+quoteRev(child)+")", "-T", "commit_id")
	jjTest(t, f.repo, "rebase", "--ignore-immutable", "-s", roots, "-d", quoteRev(parentTip)+"-")
	jjTest(t, f.repo, "new", quoteRev(child))
	tipStillPresent, err := f.repo.revisions(f.ctx, quoteRev(parentTip)+" & ::"+quoteRev(child))
	if err != nil || len(tipStillPresent) != 0 {
		t.Fatalf("fixture did not remove the old parent tip: %v %v", tipStillPresent, err)
	}
	err = f.repo.submit(f.ctx, submitOptions{bookmark: child, title: "feat(rjc): land the child independently", bodyFile: f.bodyFile}, f, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "parent-boundary") {
		t.Fatalf("interior parent work was accepted: %v", err)
	}
	if len(f.pushes) != 0 {
		t.Fatal("interior parent work reached engine")
	}
}
