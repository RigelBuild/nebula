package main

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestSubmitRefusesAllEmptyStack(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	title := "feat(rjc): submit an empty change"
	bookmark := f.new(t, "EX-1", title, "")
	if diff := jjTest(t, f.repo, "diff", "--from", quoteRev(mainOrigin), "--to", quoteRev(bookmark), "--name-only"); diff != "" {
		t.Fatalf("fixture stack has changes: %s", diff)
	}
	before := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: title, bodyFile: f.bodyFile}, f, io.Discard)
	assertRefusalGate(t, err, "empty-change")
	if len(f.pushes) != 0 || len(f.prs) != 0 {
		t.Fatalf("empty change reached push or forge write: pushes=%v prs=%v", f.pushes, f.prs)
	}
	if after := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id"); after != before {
		t.Fatalf("bookmark moved: %s -> %s", before, after)
	}
}

func TestSubmitAllowsStackWithOneEmptyCommit(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	title := "feat(rjc): keep a stack with an empty commit"
	bookmark := f.new(t, "EX-1", title, "")
	f.commit(t, "real.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	stack := quoteRev(mainOrigin) + ".." + quoteRev(bookmark)
	if empty := jjTest(t, f.repo, "log", "--no-graph", "-r", "("+stack+") & empty()", "-T", `commit_id ++ "\n"`); len(strings.Fields(empty)) != 1 {
		t.Fatalf("fixture wants one empty commit in the stack, got %q", empty)
	}
	if diff := jjTest(t, f.repo, "diff", "--from", quoteRev(mainOrigin), "--to", quoteRev(bookmark), "--name-only"); diff == "" {
		t.Fatal("fixture stack has no total diff")
	}
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: title, bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(f.pushes) != 1 || f.pushes[0] != bookmark {
		t.Fatalf("stack with one empty commit did not push: pushes=%v", f.pushes)
	}
}

func TestSubmitRefusesEmptyStackAfterBaseAdvances(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	title := "feat(rjc): refuse empty change after main moves"
	bookmark := f.new(t, "EX-1", title, "")
	advanceRemoteMain(t, f, "main-moved.txt")
	err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: title, bodyFile: f.bodyFile}, f, io.Discard)
	assertRefusalGate(t, err, "empty-change")
	if len(f.pushes) != 0 || len(f.prs) != 0 {
		t.Fatalf("empty change reached push or forge write after base advanced: pushes=%v prs=%v", f.pushes, f.prs)
	}
}

func TestSubmitRefusesEmptyMiddleBookmark(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	middle := f.new(t, "EX-1", "feat(rjc): empty middle bookmark", "")
	child := f.new(t, "EX-2", "feat(rjc): real child change", middle)
	f.commit(t, "child.txt")
	jjTest(t, f.repo, "bookmark", "move", child, "--to", "@-")
	err := f.repo.submit(f.ctx, submitOptions{bookmark: child, title: "feat(rjc): real child change", bodyFile: f.bodyFile}, f, io.Discard)
	assertRefusalGate(t, err, "empty-change")
	if !strings.Contains(err.Error(), middle) {
		t.Fatalf("refusal does not name the empty middle bookmark %s: %v", middle, err)
	}
	if len(f.pushes) != 0 || len(f.prs) != 0 {
		t.Fatalf("empty middle bookmark reached push or forge write: pushes=%v prs=%v", f.pushes, f.prs)
	}
}

// advanceRemoteMain pushes a new main commit to the bare remote from a second
// working copy, so the test repo's bookmark and @ stay where they are. submit's
// own fetch then moves main@origin, which is the base advance under test.
func advanceRemoteMain(t *testing.T, f *fixture, path string) {
	t.Helper()
	other := *f.repo
	other.root = t.TempDir()
	jjTest(t, &other, "git", "init", "--colocate")
	jjTest(t, &other, "git", "remote", "add", "origin", remoteURL(t, f.repo))
	jjTest(t, &other, "git", "fetch", "--remote", "origin")
	jjTest(t, &other, "bookmark", "track", "main@origin")
	jjTest(t, &other, "new", mainOrigin)
	writeTestFile(t, filepath.Join(other.root, path), "committed\n")
	jjTest(t, &other, "commit", "-m", "chore: advance main on remote")
	jjTest(t, &other, "bookmark", "set", "main", "-r", "@-")
	jjTest(t, &other, "git", "push", "--bookmark", "main")
}

// remoteURL returns the origin URL of the test repo.
func remoteURL(t *testing.T, r *repository) string {
	t.Helper()
	for line := range strings.SplitSeq(jjTest(t, r, "git", "remote", "list"), "\n") {
		if name, url, ok := strings.Cut(line, " "); ok && name == "origin" {
			return strings.TrimSpace(url)
		}
	}
	t.Fatal("test repo has no origin remote")
	return ""
}
