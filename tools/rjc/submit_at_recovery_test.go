package main

import (
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// TestSubmitAtConflictRecoveryKeepsRebuiltChild submits a rebuilt stacked child
// with --at, moves origin trunk so the parent conflicts, runs the printed
// recovery commands, and checks that the rebuilt commit is still reachable from
// a bookmark.
func TestSubmitAtConflictRecoveryKeepsRebuiltChild(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	parent := f.new(t, "EX-1", "feat(rjc): parent line", "")
	f.commit(t, "parent-old.txt")
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: parent, title: "feat(rjc): parent line", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	child, err := f.repo.newChange(f.ctx, newOptions{issue: "EX-2", title: "feat(rjc): child line", on: parent})
	if err != nil {
		t.Fatal(err)
	}
	f.commit(t, "child-old.txt")
	jjTest(t, f.repo, "bookmark", "move", child, "--to", "@-")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: child, title: "feat(rjc): child line", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	oldParent := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(parent), "-T", "commit_id")
	jjTest(t, f.repo, "new", quoteRev(oldParent))
	f.commit(t, "child-rebuilt.txt")
	rebuilt := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	jjTest(t, f.repo, "new", mainOrigin)
	writeTestFile(t, filepath.Join(f.repo.root, "parent-old.txt"), "trunk\n")
	jjTest(t, f.repo, "commit", "-m", "fix: trunk conflicts with parent")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	err = f.repo.submit(f.ctx, submitOptions{bookmark: child, at: rebuilt, title: "feat(rjc): child line", bodyFile: f.bodyFile}, f, io.Discard)
	assertRefusalGate(t, err, "conflict")
	detail, ok := errors.AsType[*failure](err)
	if !ok {
		t.Fatalf("want rebase failure with recovery, got %v", err)
	}
	runPrintedRecovery(t, f, detail.fix)
	got := jjTest(t, f.repo, "log", "--no-graph", "-r", "("+quoteRev(rebuilt)+") & ::bookmarks()", "-T", "commit_id")
	if got != rebuilt {
		t.Fatalf("rebuilt commit %s is not reachable from a bookmark after recovery (got %q); fix:\n%s", rebuilt, got, detail.fix)
	}
}

// runPrintedRecovery runs each jj line of a printed recovery, as a user would.
func runPrintedRecovery(t *testing.T, f *fixture, fix string) {
	t.Helper()
	for line := range strings.SplitSeq(fix, "\n") {
		args := strings.Fields(line)
		if len(args) < 2 || args[0] != "jj" {
			continue
		}
		jjTest(t, f.repo, args[1:]...)
	}
}
