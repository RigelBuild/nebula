package main

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func remoteParentAdoptionFixture(t *testing.T) (*fixture, string, string, string) {
	t.Helper()
	f := parallelFixtureRepo(t)
	parent := f.new(t, "EX-1", "feat(rjc): parent work before adoption", "")
	f.commit(t, "parent.txt")
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
	foreign := strings.Replace(parent, "test-lane/", "other-lane/", 1)
	jjTest(t, f.repo, "bookmark", "rename", parent, foreign)
	jjTest(t, f.repo, "git", "push", "--bookmark", foreign)
	parentTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(foreign)+"@origin", "-T", "commit_id")
	f.mergedHeads = map[string]string{foreign: parentTip}
	jjTest(t, f.repo, "bookmark", "untrack", foreign+"@origin")
	jjTest(t, f.repo, "bookmark", "forget", foreign)
	jjTest(t, f.repo, "new", quoteRev(foreign)+"@origin")
	f.commit(t, "child.txt")
	at := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	return f, foreign, parentTip, at
}

func TestAdoptionRequiresExplicitRemoteParent(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"new --at", "submit --issue"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f, parent, parentTip, at := remoteParentAdoptionFixture(t)
			title := "feat(rjc): adopt an existing foreign child"
			child, err := f.repo.keys.BookmarkName(f.repo.codename, 2, title)
			if err != nil {
				t.Fatal(err)
			}
			var adoptionErr error
			if mode == "new --at" {
				_, adoptionErr = f.repo.newChange(f.ctx, newOptions{issue: "EX-2", title: title, at: quoteRev(at)})
			} else {
				adoptionErr = f.repo.submit(f.ctx, submitOptions{issue: "EX-2", title: title, bodyFile: f.bodyFile, dryRun: true}, f, io.Discard)
			}
			if adoptionErr != nil {
				assertRefusalGate(t, adoptionErr, "remote-parent")
				detail, ok := errors.AsType[*failure](adoptionErr)
				if !ok || !strings.Contains(detail.fix, "rjc new --on <bookmark>") {
					t.Fatalf("missing explicit-parent remedy: %v", adoptionErr)
				}
				after := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
				if after != at {
					t.Fatal("refusal rewrote existing work")
				}
				bookmarks, err := f.repo.revisions(f.ctx, "bookmarks(exact:"+quoteRev(child)+")")
				if err != nil || len(bookmarks) != 0 {
					t.Fatalf("refusal created an owned bookmark: %v %v", bookmarks, err)
				}
			}
			f.prs[parent] = 1
			f.merged = map[string]bool{parent: true}
			jjTest(t, f.repo, "new", mainOrigin)
			f.commit(t, "parent.txt")
			jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
			jjTest(t, f.repo, "git", "push", "--bookmark", "main")
			jjTest(t, f.repo, "bookmark", "create", parent, "-r", quoteRev(parentTip))
			jjTest(t, f.repo, "bookmark", "track", parent+"@origin")
			deleteParentBranch(t, f, parent)
			jjTest(t, f.repo, "new", quoteRev(at))
			submitErr := f.repo.submit(f.ctx, submitOptions{bookmark: child, title: title, bodyFile: f.bodyFile}, f, io.Discard)
			if adoptionErr == nil || submitErr == nil || len(f.pushes) != 0 {
				t.Fatalf("remote parent adoption escaped: adoption=%v submit=%v pushes=%v", adoptionErr, submitErr, f.pushes)
			}
		})
	}
}

func TestExplicitParentAdoptionSavesRelationship(t *testing.T) {
	t.Parallel()
	f, parent, parentTip, at := remoteParentAdoptionFixture(t)
	child, err := f.repo.newChange(f.ctx, newOptions{issue: "EX-2", title: "feat(rjc): explicitly adopt existing child work", on: parent, at: quoteRev(at)})
	if err != nil {
		t.Fatal(err)
	}
	saved := savedParent(t, f, child)
	if saved.Bookmark != parent || saved.Tip != parentTip || len(persistedParentChangeIDs(t, f, child)) == 0 {
		t.Fatalf("explicit adoption lost parent state: %+v", saved)
	}
	line, err := f.repo.line(f.ctx, child, lineOptions{})
	if err != nil || len(line) != 1 || line[0].Base != parent+"@origin" {
		t.Fatalf("explicit adoption line: %v, %v", line, err)
	}
	if len(f.pushes) != 0 {
		t.Fatalf("adoption pushed work: %v", f.pushes)
	}
}
