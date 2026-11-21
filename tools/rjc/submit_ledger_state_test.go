package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestSubmitLedgerPRLookupErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "missing ledger PR allows submit", status: http.StatusNotFound},
		{name: "server error refuses submit", status: http.StatusInternalServerError, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := parallelFixtureRepo(t)
			bookmark := f.new(t, "EX-1", "feat(rjc): handle missing ledger PR", "")
			f.commit(t, "feature.txt")

			const stalePR = 999
			if err := (Ledger{root: f.repo.stateDir}).Save(LedgerEntry{
				Repo:     f.repo.profile.Name,
				PR:       stalePR,
				Bookmark: bookmark,
				Codename: f.repo.codename,
			}); err != nil {
				t.Fatal(err)
			}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet && request.URL.Path == prPath(f.repo.profile.Name, stalePR) {
					w.WriteHeader(tt.status)
					respondJSON(t, w, map[string]string{"message": http.StatusText(tt.status)})
					return
				}
				f.serve(t, w, request)
			}))
			t.Cleanup(server.Close)

			s, err := service(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			s.forge = &forgeClient{client: server.Client(), baseURL: server.URL}

			err = f.repo.submit(f.ctx, submitOptions{
				bookmark: bookmark,
				title:    "feat(rjc): handle missing ledger PR",
				bodyFile: f.bodyFile,
			}, f, io.Discard)
			if tt.wantErr {
				failure, ok := errors.AsType[*forgeAPIError](err)
				if !ok || failure.status != tt.status {
					t.Fatalf("submit error = %v, want forge HTTP %d error", err, tt.status)
				}
				if len(f.pushes) != 0 {
					t.Fatalf("submit pushed after ledger lookup failure: %v", f.pushes)
				}
				return
			}
			if err != nil {
				t.Fatalf("submit with missing ledger PR: %v", err)
			}
			if len(f.pushes) != 1 || f.pushes[0] != bookmark {
				t.Fatalf("submit pushes = %v, want [%s]", f.pushes, bookmark)
			}
		})
	}
}

// unpublishedLineWithDescendant returns an unpublished owned line and a
// described child of its tip that no bookmark names.
func unpublishedLineWithDescendant(t *testing.T, f *fixture) ([]Change, string) {
	t.Helper()
	bookmark := f.new(t, "EX-1", "feat(rjc): excuse recorded head", "")
	f.commit(t, "owned.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	f.commit(t, "descendant.txt")
	descendant := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	return []Change{{Bookmark: bookmark, Tip: tip, Base: mainOrigin, Owned: true}}, descendant
}

func TestForeignDescendantsExcusesRecordedOwnHead(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	line, descendant := unpublishedLineWithDescendant(t, f)
	f.prs[line[0].Bookmark] = 1
	if err := (Ledger{root: f.repo.stateDir}).Save(LedgerEntry{Repo: f.repo.profile.Name, PR: 1, Bookmark: line[0].Bookmark, Codename: f.repo.codename, LastPushedHead: descendant}); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.checkLinearOwnership(f.ctx, line, false); err != nil {
		t.Fatalf("descendant recorded as own pushed head was refused: %v", err)
	}
}

func TestForeignDescendantsRefusesUnrecordedDescendant(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	line, _ := unpublishedLineWithDescendant(t, f)
	assertRefusalGate(t, f.repo.checkLinearOwnership(f.ctx, line, false), "foreign-descendant")
}

func TestForeignDescendantsRefusesHeadRecordedForOtherPR(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	line, descendant := unpublishedLineWithDescendant(t, f)
	bookmark := line[0].Bookmark
	f.prs[bookmark] = 1
	if err := (Ledger{root: f.repo.stateDir}).Save(LedgerEntry{Repo: f.repo.profile.Name, PR: 5, Bookmark: bookmark, Codename: f.repo.codename, LastPushedHead: descendant}); err != nil {
		t.Fatal(err)
	}
	assertRefusalGate(t, f.repo.checkLinearOwnership(f.ctx, line, false), "foreign-descendant")
}

// pushLineHeadsThenRevive submits the owned line, then commits a child on its
// tip and records that child as a head rjc pushed for the PR. The child is left
// as a local, unbookmarked descendant of the line, as in the logged incident, so
// the descendant check (not the published check) is what sees it. It returns the
// bookmark and the recorded child head.
func pushLineHeadsThenRevive(t *testing.T, f *fixture) (bookmark, first string) {
	t.Helper()
	bookmark = f.new(t, "EX-1", "feat(rjc): revive old heads", "")
	f.commit(t, "owned.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: "feat(rjc): revive old heads", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	f.commit(t, "old-child.txt")
	first = jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	if err := (Ledger{root: f.repo.stateDir}).recordPush(f.repo.profile.Name, bookmark, 1, f.repo.codename, first); err != nil {
		t.Fatal(err)
	}
	return bookmark, first
}

// TestSubmitIncidentRevivedOldHeadsRebasesWithoutDivergence reproduces the
// logged incident: earlier pushed heads of one PR come back into view after a
// fetch, then a real submit rebases the line onto a moved main.
func TestSubmitIncidentRevivedOldHeadsRebasesWithoutDivergence(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark, _ := pushLineHeadsThenRevive(t, f)
	f.prs[bookmark] = 1

	jjTest(t, f.repo, "new", mainOrigin)
	f.commit(t, "unrelated.txt")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	jjTest(t, f.repo, "new", quoteRev(bookmark))

	if err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: "feat(rjc): revive old heads", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatalf("real submit refused after revived old heads: %v", err)
	}
	if divergent := jjTest(t, f.repo, "log", "--no-graph", "-r", "divergent()", "-T", `commit_id ++ "\n"`); strings.TrimSpace(divergent) != "" {
		t.Fatalf("submit left divergent commits: %s", divergent)
	}
}

// TestForeignDescendantsRefusesChildOfRecordedHead checks that work someone
// built on an old pushed head still refuses, even when that head is recorded.
func TestForeignDescendantsRefusesChildOfRecordedHead(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark, first := pushLineHeadsThenRevive(t, f)
	f.prs[bookmark] = 1
	jjTest(t, f.repo, "new", quoteRev(first))
	f.commit(t, "built-on-old.txt")
	line := []Change{{Bookmark: bookmark, Tip: jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id"), Base: mainOrigin, Owned: true}}
	assertRefusalGate(t, f.repo.checkLinearOwnership(f.ctx, line, false), "foreign-descendant")
}

// TestSubmitKeepsEarlierPushedCommitsInLine checks that a PR pushed twice keeps
// the commits of its first push when main moves and the line rebases. The first
// pushed head is an interior commit of the line, so the excuse must not abandon it.
func TestSubmitKeepsEarlierPushedCommitsInLine(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	title := "feat(rjc): keep earlier pushed commits"
	bookmark := f.new(t, "EX-1", title, "")
	f.commit(t, "first.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: title, bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	f.commit(t, "second.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: title, bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	jjTest(t, f.repo, "new", mainOrigin)
	f.commit(t, "unrelated.txt")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	jjTest(t, f.repo, "new", quoteRev(bookmark))
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: title, bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatalf("real submit refused after the PR was pushed twice: %v", err)
	}
	files := jjTest(t, f.repo, "file", "list", "-r", quoteRev(bookmark))
	for _, want := range []string{"first.txt", "second.txt"} {
		if !strings.Contains(files, want) {
			t.Fatalf("%s left the line after rebase; files at tip: %q", want, files)
		}
	}
}

// TestSubmitExcusesAncestorsOfRecordedOldHead checks that an old pushed head two
// commits deep stays excusable after main moves. The commit under the recorded
// head is part of the history rjc pushed for the PR, so it is not foreign work.
func TestSubmitExcusesAncestorsOfRecordedOldHead(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	title := "feat(rjc): excuse two-commit old head"
	bookmark := f.new(t, "EX-1", title, "")
	f.commit(t, "owned.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: title, bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	f.commit(t, "old-one.txt")
	f.commit(t, "old-two.txt")
	oldHead := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	if err := (Ledger{root: f.repo.stateDir}).recordPush(f.repo.profile.Name, bookmark, 1, f.repo.codename, oldHead); err != nil {
		t.Fatal(err)
	}
	jjTest(t, f.repo, "new", mainOrigin)
	f.commit(t, "unrelated.txt")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	jjTest(t, f.repo, "new", quoteRev(bookmark))
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: title, bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatalf("real submit refused after a two-commit old head: %v", err)
	}
	files := jjTest(t, f.repo, "file", "list", "-r", quoteRev(bookmark))
	for _, gone := range []string{"old-one.txt", "old-two.txt"} {
		if strings.Contains(files, gone) {
			t.Fatalf("%s survived the rebase; files at tip: %q", gone, files)
		}
	}
	if !strings.Contains(files, "owned.txt") {
		t.Fatalf("owned.txt left the line; files at tip: %q", files)
	}
}

// TestSubmitRefusesForeignCommitAboveRecordedOldHead checks that a commit on
// top of a recorded old head still refuses. The excuse widens only to the
// history under a head, never to its descendants.
func TestSubmitRefusesForeignCommitAboveRecordedOldHead(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	title := "feat(rjc): refuse work above old head"
	bookmark := f.new(t, "EX-1", title, "")
	f.commit(t, "owned.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: title, bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	f.commit(t, "old-one.txt")
	f.commit(t, "old-two.txt")
	oldHead := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	if err := (Ledger{root: f.repo.stateDir}).recordPush(f.repo.profile.Name, bookmark, 1, f.repo.codename, oldHead); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(f.repo.root, "foreign.txt"), "foreign\n")
	jjTest(t, f.repo, "commit", "-m", "feat: foreign work above the old head")
	jjTest(t, f.repo, "new", mainOrigin)
	f.commit(t, "unrelated.txt")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	jjTest(t, f.repo, "new", quoteRev(bookmark))
	err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: title, bodyFile: f.bodyFile}, f, io.Discard)
	assertRefusalGate(t, err, "foreign-descendant")
}

// TestForeignDescendantsRefusesAncestorNamedByOtherBookmark checks that an
// ancestor of a recorded old head still refuses when another bookmark names it.
// That commit belongs to another line, so the excuse must not reach it.
func TestForeignDescendantsRefusesAncestorNamedByOtherBookmark(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): guard other line", "")
	f.commit(t, "owned.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	f.commit(t, "old-one.txt")
	oldOne := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	f.commit(t, "old-two.txt")
	oldTwo := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	if err := (Ledger{root: f.repo.stateDir}).recordPush(f.repo.profile.Name, bookmark, 1, f.repo.codename, oldTwo); err != nil {
		t.Fatal(err)
	}
	f.prs[bookmark] = 1
	jjTest(t, f.repo, "bookmark", "create", "test-lane/other", "-r", oldOne)
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	line := []Change{{Bookmark: bookmark, Tip: tip, Base: mainOrigin, Owned: true}}
	assertRefusalGate(t, f.repo.checkLinearOwnership(f.ctx, line, false), "foreign-descendant")
}

// unknownRecordedHead is a commit id this clone does not have. A PR's ledger
// can hold such a head when another clone force-pushed over it.
const unknownRecordedHead = "0123456789abcdef0123456789abcdef01234567"

// TestSubmitUnknownRecordedHeadRebases checks that a recorded head this clone
// does not have does not fail a real submit that rebases onto a moved main. jj
// reads a bare unknown commit id as an error, so the excuse must name each head
// with present().
func TestSubmitUnknownRecordedHeadRebases(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	title := "feat(rjc): skip unknown recorded head"
	bookmark := f.new(t, "EX-1", title, "")
	f.commit(t, "owned.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: title, bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	f.prs[bookmark] = 1
	if err := (Ledger{root: f.repo.stateDir}).recordPush(f.repo.profile.Name, bookmark, 1, f.repo.codename, unknownRecordedHead); err != nil {
		t.Fatal(err)
	}
	jjTest(t, f.repo, "new", mainOrigin)
	f.commit(t, "unrelated.txt")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	jjTest(t, f.repo, "new", quoteRev(bookmark))
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: title, bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatalf("real submit failed with an unknown recorded head: %v", err)
	}
}

// TestForeignDescendantsRefusesWithUnknownRecordedHead checks that the ownership
// check reports the foreign descendant, not a jj error, when a recorded head is
// missing from this clone.
func TestForeignDescendantsRefusesWithUnknownRecordedHead(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	line, _ := unpublishedLineWithDescendant(t, f)
	bookmark := line[0].Bookmark
	f.prs[bookmark] = 1
	if err := (Ledger{root: f.repo.stateDir}).recordPush(f.repo.profile.Name, bookmark, 1, f.repo.codename, unknownRecordedHead); err != nil {
		t.Fatal(err)
	}
	assertRefusalGate(t, f.repo.checkLinearOwnership(f.ctx, line, false), "foreign-descendant")
}

// submitWithEmptyWorkingCopyChild builds a PR whose tip has an empty, undescribed
// working-copy child (jj's @), then submits. child runs before the submit and
// may describe or fill the child. The returned error is the submit result.
func submitWithEmptyWorkingCopyChild(t *testing.T, child func(f *fixture)) error {
	t.Helper()
	f := parallelFixtureRepo(t)
	title := "feat(rjc): empty working-copy child"
	bookmark := f.new(t, "EX-1", title, "")
	f.commit(t, "owned.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: title, bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	f.prs[bookmark] = 1
	jjTest(t, f.repo, "new", mainOrigin)
	f.commit(t, "unrelated.txt")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	jjTest(t, f.repo, "new", quoteRev(bookmark))
	child(f)
	return f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: title, bodyFile: f.bodyFile}, f, io.Discard)
}

// TestSubmitAllowsEmptyWorkingCopyChild checks that an empty, undescribed
// working-copy commit above the tip, which jj creates, does not block a submit.
func TestSubmitAllowsEmptyWorkingCopyChild(t *testing.T) {
	t.Parallel()
	if err := submitWithEmptyWorkingCopyChild(t, func(f *fixture) {}); err != nil {
		t.Fatalf("submit refused with an empty working-copy child: %v", err)
	}
}

// TestSubmitRefusesDescribedChildAboveTip checks that a described, empty child
// of the tip beside the working copy still refuses. rjc did not create it, so
// only jj's empty, undescribed commits are excused. The child is a sibling of
// @, because a submit adopts the commits under @ onto the line.
func TestSubmitRefusesDescribedChildAboveTip(t *testing.T) {
	t.Parallel()
	err := submitWithEmptyWorkingCopyChild(t, func(f *fixture) {
		jjTest(t, f.repo, "describe", "-m", "feat(rjc): described child")
		jjTest(t, f.repo, "new", "@-")
	})
	assertRefusalGate(t, err, "foreign-descendant")
}

// TestSubmitRefusesNonEmptyChildAboveTip checks that a child of the tip with a
// file change, beside the working copy, still refuses.
func TestSubmitRefusesNonEmptyChildAboveTip(t *testing.T) {
	t.Parallel()
	err := submitWithEmptyWorkingCopyChild(t, func(f *fixture) {
		f.commit(t, "child.txt")
		jjTest(t, f.repo, "new", "@--")
	})
	assertRefusalGate(t, err, "foreign-descendant")
}
