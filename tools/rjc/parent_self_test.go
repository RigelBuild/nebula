package main

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

const selfParentGate = "self-parent"

func selfParentStatePath(t *testing.T, f *fixture, bookmark string) string {
	t.Helper()
	path, err := (fileBaseline{root: f.repo.stateDir, keys: f.repo.keys}).path(bookmark, ".parent.json")
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// assertSelfParentGate checks the refusal gate, exit code and state path.
func assertSelfParentGate(t *testing.T, err error, path string) {
	t.Helper()
	got, ok := errors.AsType[*failure](err)
	if !ok {
		t.Fatalf("self-parent error is not a refusal: %v", err)
	}
	if got.gate != selfParentGate || got.code != 1 {
		t.Fatalf("gate = %q code = %d, want %q code 1", got.gate, got.code, selfParentGate)
	}
	if !strings.Contains(got.fix, path) {
		t.Fatalf("fix %q does not name state path %s", got.fix, path)
	}
}

func TestNewRefusesSelfParentWhenBookmarkExistsOnlyAtOrigin(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	title := "feat(rjc): refuse a self parent from origin"
	bookmark := f.new(t, "EX-4", title, "")
	jjTest(t, f.repo, "git", "push", "--bookmark", bookmark)
	jjTest(t, f.repo, "bookmark", "untrack", bookmark+"@origin")
	jjTest(t, f.repo, "bookmark", "delete", bookmark)
	before := jjTest(t, f.repo, "log", "--no-graph", "-r", "@", "-T", "commit_id")
	var out bytes.Buffer
	code := run(f.ctx, []string{"new", "--issue", "EX-4", "--title", title, "--on", bookmark}, &out, &out)
	if code != 1 || !strings.Contains(out.String(), selfParentGate) {
		t.Fatalf("new exit = %d, output = %s; want exit 1 with gate %s", code, &out, selfParentGate)
	}
	local, err := f.repo.revisions(f.ctx, "bookmarks(exact:"+quoteRev(bookmark)+")")
	if err != nil {
		t.Fatal(err)
	}
	if len(local) != 0 {
		t.Fatalf("local bookmark created before refusal: %v", local)
	}
	if after := jjTest(t, f.repo, "log", "--no-graph", "-r", "@", "-T", "commit_id"); after != before {
		t.Fatalf("working copy changed before refusal: %s -> %s", before, after)
	}
	if _, err := os.Stat(selfParentStatePath(t, f, bookmark)); !os.IsNotExist(err) {
		t.Fatalf("self-parent state was written: %v", err)
	}
}

func TestSaveParentRefusesSelfParent(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-5", "feat(rjc): refuse a self parent write", "")
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	err := f.repo.saveParent(f.ctx, bookmark, parentRelationship{Bookmark: bookmark, Tip: tip})
	assertSelfParentGate(t, err, selfParentStatePath(t, f, bookmark))
	if _, err := os.Stat(selfParentStatePath(t, f, bookmark)); !os.IsNotExist(err) {
		t.Fatalf("self-parent state was written: %v", err)
	}
}

func TestSyncRefusesSavedSelfParent(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-6", "feat(rjc): refuse a saved self parent", "")
	f.commit(t, "self.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	path := selfParentStatePath(t, f, bookmark)
	writeTestFile(t, path, `{"bookmark":"`+bookmark+`","tip":"`+tip+`"}`)
	var out bytes.Buffer
	if code := run(f.ctx, []string{"sync", bookmark}, &out, &out); code != 1 || !strings.Contains(out.String(), selfParentGate) {
		t.Fatalf("sync exit = %d, output = %s; want exit 1 with gate %s", code, &out, selfParentGate)
	}
	if !strings.Contains(out.String(), path) {
		t.Fatalf("sync output does not name state path %s: %s", path, &out)
	}
}

func TestSubmitRefusesSavedSelfParent(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	title := "feat(rjc): refuse a saved self parent on submit"
	bookmark := f.new(t, "EX-7", title, "")
	f.commit(t, "self.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	path := selfParentStatePath(t, f, bookmark)
	writeTestFile(t, path, `{"bookmark":"`+bookmark+`","tip":"`+tip+`"}`)
	var out bytes.Buffer
	args := []string{"submit", bookmark, "--title", title, "--body-file", f.bodyFile, "--dry-run"}
	if code := run(f.ctx, args, &out, &out); code != 1 || !strings.Contains(out.String(), selfParentGate) {
		t.Fatalf("submit exit = %d, output = %s; want exit 1 with gate %s", code, &out, selfParentGate)
	}
	if !strings.Contains(out.String(), path) {
		t.Fatalf("submit output does not name state path %s: %s", path, &out)
	}
	if len(f.pushes) != 0 {
		t.Fatalf("self-parent state reached engine: %v", f.pushes)
	}
}
