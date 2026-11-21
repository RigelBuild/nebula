package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLifecycleRoundRejectsNonAncestor(t *testing.T) {
	l := newLifecycleFixture(t)
	file := filepath.Join(t.TempDir(), "round.txt")
	if err := os.WriteFile(file, []byte("review"), 0600); err != nil {
		t.Fatal(err)
	}
	other := jjTest(t, l.f.repo, "log", "--no-graph", "-r", "@", "-T", "commit_id")
	code, out := l.command(t, "note", l.bookmark, "--round", file, "--reviewed", other)
	if code != 1 || l.posts != 0 || !strings.Contains(out, "not-ancestor") {
		t.Fatalf("%d %s", code, out)
	}
}

func TestLifecycleMissingLedgerAndEvidence(t *testing.T) {
	l := newLifecycleFixture(t)
	l.clear("agent", l.head, "main")
	ledger := Ledger{root: l.f.repo.stateDir}
	entry, err := ledger.path(l.f.repo.profile.Name, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(entry); err != nil {
		t.Fatal(err)
	}
	code, out := l.command(t, "ready", l.bookmark)
	if code != 1 || !strings.Contains(out, "not-in-ledger") {
		t.Fatalf("%d %s", code, out)
	}
	if err := ledger.Save(LedgerEntry{Repo: l.f.repo.profile.Name, PR: 1, Bookmark: l.bookmark, Codename: l.f.repo.codename}); err != nil {
		t.Fatal(err)
	}
	store := localPrePush{root: l.f.repo.stateDir}
	path, err := store.path(l.f.repo.profile.Name, l.head)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	code, out = l.command(t, "ready", l.bookmark)
	if code != 1 || !strings.Contains(out, "hooks-not-passed") {
		t.Fatalf("%d %s", code, out)
	}
}

func TestLifecycleReadyRefusesMovedBase(t *testing.T) {
	l := newLifecycleFixture(t)
	l.clear("agent", l.head, "main")
	meta := l.f.metadata[1]
	meta.Draft = false
	meta.BaseRef = "other"
	l.f.metadata[1] = meta
	code, out := l.command(t, "ready", l.bookmark)
	if code != 1 || l.promotions != 0 || !strings.Contains(out, "base-mismatch") {
		t.Fatalf("%d %s", code, out)
	}
}

func TestLifecycleClearAllowsOwnedPublishedChild(t *testing.T) {
	l := newLifecycleFixture(t)
	jjTest(t, l.f.repo, "new", quoteRev(l.bookmark), "-m", "feat: owned child")
	writeTestFile(t, filepath.Join(l.f.repo.root, "child.txt"), "child\n")
	child := l.f.repo.codename + "/ex-2-child"
	jjTest(t, l.f.repo, "bookmark", "create", child, "-r", "@")
	jjTest(t, l.f.repo, "git", "push", "--bookmark", child)
	jjTest(t, l.f.repo, "bookmark", "forget", child)
	code, out := l.command(t, "note", l.bookmark, "--clear", "--reviewed", l.head)
	if code != 0 || l.posts != 1 || l.promotions != 1 {
		t.Fatalf("code=%d posts=%d promotions=%d output=%s", code, l.posts, l.promotions, out)
	}
}

func TestLifecycleNoteRefusesForeignBookmark(t *testing.T) {
	l := newLifecycleFixture(t)
	jjTest(t, l.f.repo, "bookmark", "create", "other-lane/ex-2-lower", "-r", l.bookmark)
	code, out := l.command(t, "note", "other-lane/ex-2-lower", "--clear", "--reviewed", l.head)
	if code != 1 || l.posts != 0 || !strings.Contains(out, "foreign-bookmark") {
		t.Fatalf("code=%d posts=%d output=%s", code, l.posts, out)
	}
}
