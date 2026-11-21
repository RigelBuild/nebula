package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestNewUsesLiteralBookmarkForWorkingCopy(t *testing.T) {
	f := fixtureRepo(t)
	foreign := f.new(t, "EX-1", "feat(rjc): unrelated alias destination", "")
	title := "feat(rjc): create literal working copy"
	bookmark, err := f.repo.keys.BookmarkName(f.repo.codename, 2, title)
	if err != nil {
		t.Fatal(err)
	}
	jjTest(t, f.repo, "config", "set", "--workspace", `revset-aliases."`+bookmark+`"`, quoteRev(foreign))
	created := f.new(t, "EX-2", title, "")
	if created != bookmark {
		t.Fatalf("bookmark = %q, want %q", created, bookmark)
	}
	want := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	if got := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id"); got != want {
		t.Fatalf("working copy parent = %s, want %s", got, want)
	}
}

func TestFailedCreationDoesNotSaveParentAndMainReuseClearsState(t *testing.T) {
	f := fixtureRepo(t)
	parent := f.new(t, "EX-1", "feat(rjc): parent for failed creation", "")
	jjTest(t, f.repo, "git", "push", "--bookmark", parent)
	installJJProxy(t)
	t.Setenv("RJC_TEST_FAIL_BOOKMARK", "1")
	title := "feat(rjc): reuse a failed bookmark name"
	bookmark, err := f.repo.keys.BookmarkName(f.repo.codename, 2, title)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.newChange(f.ctx, newOptions{issue: "EX-2", title: title, on: parent}); err == nil {
		t.Fatal("fixture bookmark creation did not fail")
	}
	path, err := (fileBaseline{root: f.repo.stateDir, keys: f.repo.keys}).path(bookmark, ".parent.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("failed creation saved parent state: %v", err)
	}
	writeTestFile(t, path, `{"bookmark":"other-lane/ex-9-orphan","tip":"orphan"}`)
	t.Setenv("RJC_TEST_FAIL_BOOKMARK", "0")
	if got := f.new(t, "EX-2", title, ""); got != bookmark {
		t.Fatalf("reused bookmark = %s", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("main-based creation retained orphan state: %v", err)
	}
	line, err := f.repo.line(f.ctx, bookmark, lineOptions{})
	if err != nil || line[0].Base != mainOrigin {
		t.Fatalf("reused line = %v err=%v", line, err)
	}
}

func installJJProxy(t *testing.T) {
	t.Helper()
	real, err := exec.LookPath("jj")
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.Symlink(executable, filepath.Join(bin, "jj")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RJC_TEST_REAL_JJ", real)
	t.Setenv("RJC_TEST_HELPER", "1")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestNewExistenceProbeIsExact(t *testing.T) {
	f := fixtureRepo(t)
	title := "feat(rjc): exact bookmark creation probe"
	bookmark, err := f.repo.keys.BookmarkName(f.repo.codename, 1, title)
	if err != nil {
		t.Fatal(err)
	}
	jjTest(t, f.repo, "bookmark", "create", bookmark+"-longer", "-r", mainOrigin)
	installJJProxy(t)
	t.Setenv("RJC_TEST_EXACT_BOOKMARK", bookmark)
	if got := f.new(t, "EX-1", title, ""); got != bookmark {
		t.Fatalf("created bookmark = %q", got)
	}
}

func TestParentStateSurvivesWorkingCopyCreationFailure(t *testing.T) {
	f := fixtureRepo(t)
	parent := f.new(t, "EX-1", "feat(rjc): parent before failed disarm", "")
	jjTest(t, f.repo, "git", "push", "--bookmark", parent)
	title := "feat(rjc): preserve parent if disarm fails"
	child, err := f.repo.keys.BookmarkName(f.repo.codename, 2, title)
	if err != nil {
		t.Fatal(err)
	}
	installJJProxy(t)
	t.Setenv("RJC_TEST_FAIL_NEW_AT", quoteRev(child))
	_, err = f.repo.newChange(f.ctx, newOptions{issue: "EX-2", title: title, on: parent})
	if err == nil {
		t.Fatal("fixture working-copy creation did not fail")
	}
	if _, err := f.repo.one(f.ctx, "bookmarks(exact:"+quoteRev(child)+")"); err != nil {
		t.Fatal("bookmark was not created before the injected failure", err)
	}
	if saved := savedParent(t, f, child); saved.Bookmark != parent || saved.Tip == "" {
		t.Fatalf("created bookmark lost its parent: %+v", saved)
	}
	if len(persistedParentChangeIDs(t, f, child)) == 0 {
		t.Fatal("created bookmark lost the parent's change IDs")
	}
}
