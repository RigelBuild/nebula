package main

import (
	"encoding/json"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestRewrittenMergedParentRefusesBeforePush(t *testing.T) {
	t.Parallel()
	for _, deleted := range []bool{false, true} {
		t.Run(map[bool]string{false: "branch retained", true: "branch deleted"}[deleted], func(t *testing.T) {
			t.Parallel()
			f, parent, oldTip, child := parentFixture(t)
			jjTest(t, f.repo, "new", mainOrigin)
			f.commit(t, "main-advance.txt")
			jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
			jjTest(t, f.repo, "git", "push", "--bookmark", "main")
			roots := jjTest(t, f.repo, "log", "--no-graph", "-r", "roots(main@origin.."+quoteRev(oldTip)+")", "-T", "commit_id")
			jjTest(t, f.repo, "rebase", "--ignore-immutable", "-s", roots, "-d", mainOrigin)
			jjTest(t, f.repo, "git", "push", "--bookmark", parent)
			newTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(parent)+"@origin", "-T", "commit_id")
			if newTip == oldTip || savedParent(t, f, child).Tip != oldTip {
				t.Fatal("fixture did not rewrite the parent without refreshing saved state")
			}
			f.prs[parent] = 1
			f.merged = map[string]bool{parent: true}
			f.mergedHeads[parent] = newTip
			jjTest(t, f.repo, "new", mainOrigin)
			f.commit(t, "parent.txt")
			jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
			jjTest(t, f.repo, "git", "push", "--bookmark", "main")
			if deleted {
				deleteParentBranch(t, f, parent)
			}
			jjTest(t, f.repo, "new", quoteRev(child))
			opt := submitOptions{bookmark: child, title: "feat(rjc): land the child independently", bodyFile: f.bodyFile}
			assertParentSubmitRefuses(t, f, opt)
			if !deleted {
				deleteParentBranch(t, f, parent)
				assertParentSubmitRefuses(t, f, opt)
			}
		})
	}
}

func TestMergedParentHeadFindsReplacementWork(t *testing.T) {
	t.Parallel()
	f, parent, oldTip, child := parentFixture(t)
	jjTest(t, f.repo, "new", mainOrigin, "-m", "feat(rjc): replacement parent work")
	f.commit(t, "parent.txt")
	newTip := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", newTip, "--allow-backwards")
	roots := jjTest(t, f.repo, "log", "--no-graph", "-r", "roots("+quoteRev(oldTip)+".."+quoteRev(child)+")", "-T", "commit_id")
	jjTest(t, f.repo, "rebase", "--ignore-immutable", "-s", roots, "-d", quoteRev(newTip))
	jjTest(t, f.repo, "git", "push", "--bookmark", parent)
	f.prs[parent] = 1
	f.merged = map[string]bool{parent: true}
	f.mergedHeads[parent] = newTip
	jjTest(t, f.repo, "new", mainOrigin)
	f.commit(t, "parent.txt")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	deleteParentBranch(t, f, parent)
	jjTest(t, f.repo, "new", quoteRev(child))
	oldIDs := strings.Fields(jjTest(t, f.repo, "log", "--no-graph", "-r", "::"+quoteRev(oldTip)+" ~ ::main@origin", "-T", `change_id ++ "\n"`))
	childIDs := strings.Fields(jjTest(t, f.repo, "log", "--no-graph", "-r", "main@origin.."+quoteRev(child), "-T", `change_id ++ "\n"`))
	for _, id := range oldIDs {
		if slices.Contains(childIDs, id) {
			t.Fatal("fixture retained already-known parent work")
		}
	}
	assertParentSubmitRefuses(t, f, submitOptions{bookmark: child, title: "feat(rjc): land the child independently", bodyFile: f.bodyFile})
}

func TestMissingMergedParentHeadAllowsCleanChild(t *testing.T) {
	t.Parallel()
	f, parent, parentTip, child := parentFixture(t)
	f.prs[parent] = 1
	f.merged = map[string]bool{parent: true}
	f.mergedHeads[parent] = strings.Repeat("f", 40)
	landAndRestackChild(t, f, parent, parentTip, child, true)
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: child, title: "feat(rjc): land the child independently", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(f.pushes) != 1 || f.pushes[0] != child || f.metadata[f.prs[child]].BaseRef != "main" {
		t.Fatalf("clean child submission: pushes=%v metadata=%v", f.pushes, f.metadata)
	}
}

func TestKnownParentChangeIDsSurviveMissingForgeObject(t *testing.T) {
	t.Parallel()
	f, parent, oldTip, child := parentFixture(t)
	jjTest(t, f.repo, "new", mainOrigin)
	f.commit(t, "main-advance.txt")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	roots := jjTest(t, f.repo, "log", "--no-graph", "-r", "roots(main@origin.."+quoteRev(oldTip)+")", "-T", "commit_id")
	jjTest(t, f.repo, "rebase", "--ignore-immutable", "-s", roots, "-d", mainOrigin)
	jjTest(t, f.repo, "git", "push", "--bookmark", parent)
	f.prs[parent] = 1
	f.merged = map[string]bool{parent: true}
	f.mergedHeads[parent] = strings.Repeat("f", 40)
	deleteParentBranch(t, f, parent)
	jjTest(t, f.repo, "new", quoteRev(child))
	assertParentSubmitRefuses(t, f, submitOptions{bookmark: child, title: "feat(rjc): land the child independently", bodyFile: f.bodyFile})
}

func TestConfirmedForeignBaseKeepsParentProof(t *testing.T) {
	t.Parallel()
	f, parent, _, child := parentFixture(t)
	candidate, err := f.repo.prepareCandidate(f.ctx, child, "feat(rjc): land the child independently", f.bodyFile, "")
	if err != nil {
		t.Fatal(err)
	}
	candidate.path, err = (fileBaseline{root: f.repo.stateDir, keys: f.repo.keys}).candidate(child, candidate.body)
	if err != nil {
		t.Fatal(err)
	}
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(child), "-T", "commit_id")
	item := preparedChange{change: Change{Bookmark: child, Tip: tip, Base: parent + "@origin"}, candidate: candidate}
	s, err := service(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.store = fileBaseline{root: f.repo.stateDir, keys: f.repo.keys}
	f.repo.landedParents = map[string]string{child: parent}
	if err := f.repo.pushChange(f.ctx, &item, submitOptions{}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if savedParent(t, f, child).Bookmark != parent {
		t.Fatal("confirmed foreign base lost the selected-parent proof")
	}
}

func TestLandedParentCannotRemainLineBase(t *testing.T) {
	t.Parallel()
	f, parent, _, child := parentFixture(t)
	mainTip := jjTest(t, f.repo, "log", "--no-graph", "-r", mainOrigin, "-T", "commit_id")
	writeParentState(t, f, child, map[string]any{"bookmark": parent, "tip": mainTip, "change_ids": []string{}})
	f.prs[parent] = 1
	f.merged = map[string]bool{parent: true}
	f.mergedHeads[parent] = strings.Repeat("f", 40)
	_, err := f.repo.line(f.ctx, child, lineOptions{})
	if err == nil || !strings.Contains(err.Error(), "parent-boundary") {
		t.Fatalf("landed parent remained the line base: %v", err)
	}
	if len(f.pushes) != 0 {
		t.Fatal("landed parent boundary reached engine")
	}
}

func TestParentStateRejectsNonCommitTipsAndInvalidBookmarks(t *testing.T) {
	t.Parallel()
	for _, invalid := range []string{"main", "short", "missing", "parent-main", "parent-remote", "parent-newline"} {
		t.Run(invalid, func(t *testing.T) {
			t.Parallel()
			f, parent, parentTip, child := parentFixture(t)
			switch invalid {
			case "main":
				parentTip = "main"
			case "short":
				parentTip = parentTip[:12]
			case "missing":
				parentTip = strings.Repeat("f", 40)
			case "parent-main":
				parent = "main"
			case "parent-remote":
				parent += "@origin"
			case "parent-newline":
				parent += "\nother"
			}
			writeParentState(t, f, child, map[string]any{"bookmark": parent, "tip": parentTip})
			err := f.repo.submit(f.ctx, submitOptions{bookmark: child, title: "feat(rjc): land the child independently", bodyFile: f.bodyFile}, f, io.Discard)
			reason := "selected parent tip"
			if strings.HasPrefix(invalid, "parent-") {
				reason = "invalid selected parent bookmark"
			}
			if err == nil || !strings.Contains(err.Error(), reason) {
				t.Fatalf("invalid parent state refusal = %v", err)
			}
			if len(f.pushes) != 0 || len(f.prs) != 0 {
				t.Fatal("invalid parent state reached engine")
			}
		})
	}
}

func assertParentSubmitRefuses(t *testing.T, f *fixture, opt submitOptions) {
	t.Helper()
	err := f.repo.submit(f.ctx, opt, f, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "parent-boundary") {
		t.Fatalf("parent work was accepted: %v", err)
	}
	if len(f.pushes) != 0 {
		t.Fatalf("parent work reached engine: %v", f.pushes)
	}
	if savedParent(t, f, opt.bookmark).Bookmark == "" {
		t.Fatal("refusal lost the parent proof")
	}
}

func deleteParentBranch(t *testing.T, f *fixture, parent string) {
	t.Helper()
	jjTest(t, f.repo, "bookmark", "delete", parent)
	jjTest(t, f.repo, "git", "push", "--bookmark", parent)
}

func writeParentState(t *testing.T, f *fixture, bookmark string, state any) {
	t.Helper()
	path, err := (fileBaseline{root: f.repo.stateDir, keys: f.repo.keys}).path(bookmark, ".parent.json")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, string(data))
}

func persistedParentChangeIDs(t *testing.T, f *fixture, bookmark string) []string {
	t.Helper()
	path, err := (fileBaseline{root: f.repo.stateDir, keys: f.repo.keys}).path(bookmark, ".parent.json")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		ChangeIDs []string `json:"change_ids"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return state.ChangeIDs
}

func TestStoredChangeIDsRefuseRewrittenAlreadyLandedWork(t *testing.T) {
	t.Parallel()
	f, parent, oldTip, child := parentFixture(t)
	jjTest(t, f.repo, "describe", "--ignore-immutable", "-r", quoteRev(parent), "-m", "feat(rjc): amended parent metadata")
	newTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(parent), "-T", "commit_id")
	if newTip == oldTip {
		t.Fatal("fixture did not rewrite the parent commit")
	}
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", quoteRev(oldTip))
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	f.prs[parent] = 1
	f.merged = map[string]bool{parent: true}
	deleteParentBranch(t, f, parent)
	jjTest(t, f.repo, "new", quoteRev(child))
	oldWork := jjTest(t, f.repo, "log", "--no-graph", "-r", "::"+quoteRev(oldTip)+" ~ ::main@origin", "-T", "commit_id")
	if oldWork != "" || len(persistedParentChangeIDs(t, f, child)) == 0 {
		t.Fatal("fixture does not isolate persisted parent change IDs")
	}
	assertParentSubmitRefuses(t, f, submitOptions{bookmark: child, title: "feat(rjc): land the child independently", bodyFile: f.bodyFile})
}
