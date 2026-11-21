package main

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestAdoptionRefusesMergedDeletedParent(t *testing.T) {
	for _, mode := range []string{"new --at", "submit --issue"} {
		t.Run(mode, func(t *testing.T) {
			f, parent, parentTip, at := remoteParentAdoptionFixture(t)
			parentCommits, err := f.repo.revisions(f.ctx, "main@origin.."+quoteRev(parentTip))
			if err != nil {
				t.Fatal(err)
			}
			f.commitPRs = map[string][]string{}
			for _, sha := range parentCommits {
				f.commitPRs[sha] = []string{parent}
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
			title := "feat(rjc): refuse a deleted foreign parent"
			if mode == "new --at" {
				_, err = f.repo.newChange(f.ctx, newOptions{issue: "EX-2", title: title, at: quoteRev(at)})
			} else {
				err = f.repo.submit(f.ctx, submitOptions{issue: "EX-2", title: title, bodyFile: f.bodyFile}, f, io.Discard)
			}
			if err == nil {
				t.Fatal("merged, deleted parent work was adopted")
			}
			if got := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id"); got != at {
				t.Fatal("refusal rewrote existing work")
			}
			if len(f.pushes) != 0 {
				t.Fatalf("foreign work reached engine: %v", f.pushes)
			}
			for sha, calls := range f.commitPRCalls {
				if calls != 1 {
					t.Fatalf("commit %s queried %d times", sha, calls)
				}
			}
		})
	}
}

func TestAdoptionAllowsUnknownOwnCommit(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusUnprocessableEntity, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			f := parallelFixtureRepo(t)
			f.commit(t, "own.txt")
			f.commitPRStatus = status
			f.commitPRMessage = "No commit found for SHA: <sha>"
			at := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
			child, err := f.repo.newChange(f.ctx, newOptions{issue: "EX-2", title: "feat(rjc): adopt never-pushed own work", at: quoteRev(at)})
			if err != nil {
				t.Fatal(err)
			}
			if !f.repo.keys.Owned(f.repo.codename, child) || f.commitPRCalls[at] != 1 || f.repoReads != 1 || len(f.pushes) != 0 {
				t.Fatalf("unknown own commit: child=%s calls=%v repoReads=%d pushes=%v", child, f.commitPRCalls, f.repoReads, f.pushes)
			}
		})
	}
}

func TestAdoptionFailsClosedOnForgeErrors(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"repo missing", "server error", "auth error", "commit generic 404", "validation error", "different unknown SHA", "malformed response", "network error"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			f := parallelFixtureRepo(t)
			f.commit(t, "own.txt")
			at := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
			switch kind {
			case "repo missing":
				f.repoStatus = http.StatusNotFound
				f.commitPRStatus = http.StatusNotFound
				f.commitPRMessage = "No commit found for SHA: <sha>"
			case "server error":
				f.commitPRStatus = http.StatusInternalServerError
				f.commitPRMessage = "No commit found for SHA: <sha>"
			case "auth error":
				f.commitPRStatus = http.StatusUnauthorized
			case "commit generic 404":
				f.commitPRStatus = http.StatusNotFound
				f.commitPRMessage = "Not Found"
			case "validation error":
				f.commitPRStatus = http.StatusUnprocessableEntity
				f.commitPRMessage = "Validation Failed"
			case "different unknown SHA":
				f.commitPRStatus = http.StatusUnprocessableEntity
				f.commitPRMessage = "No commit found for SHA: " + strings.Repeat("0", 40)
			case "malformed response":
				f.commitPRMalformed = true
			case "network error":
				s, err := service(f.ctx)
				if err != nil {
					t.Fatal(err)
				}
				s.forge.client = &http.Client{Transport: failingForgeTransport{}}
			}
			_, err := f.repo.newChange(f.ctx, newOptions{issue: "EX-2", title: "feat(rjc): refuse unknown forge state", at: quoteRev(at)})
			if err == nil {
				t.Fatal("forge failure allowed adoption")
			}
			if got := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id"); got != at {
				t.Fatal("forge failure rewrote work")
			}
			if len(f.pushes) != 0 {
				t.Fatal("forge failure reached engine")
			}
			for sha, calls := range f.commitPRCalls {
				if calls != 1 {
					t.Fatalf("commit %s retried %d times", sha, calls)
				}
			}
			if kind == "repo missing" && len(f.commitPRCalls) != 0 {
				t.Fatal("repository failure was treated as an unknown commit")
			}
		})
	}
}

func TestAdoptionChecksEachOwnAssociatedCommitOnce(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	for _, file := range []string{"first.txt", "second.txt", "third.txt"} {
		f.commit(t, file)
	}
	commits, err := f.repo.revisions(f.ctx, "main@origin..@-")
	if err != nil {
		t.Fatal(err)
	}
	f.commitPRs = map[string][]string{}
	for _, sha := range commits {
		f.commitPRs[sha] = []string{"test-lane/ex-1-own-former-parent"}
	}
	if _, err := f.repo.newChange(f.ctx, newOptions{issue: "EX-2", title: "feat(rjc): adopt caller-owned associated work", at: "@-"}); err != nil {
		t.Fatal(err)
	}
	if len(commits) != 3 || len(f.commitPRCalls) != len(commits) || f.repoReads != 1 {
		t.Fatalf("lookup cardinality: commits=%v calls=%v repoReads=%d", commits, f.commitPRCalls, f.repoReads)
	}
	for _, sha := range commits {
		if f.commitPRCalls[sha] != 1 {
			t.Fatalf("commit %s queried %d times", sha, f.commitPRCalls[sha])
		}
	}
}

func TestAdoptionRejectsForeignAssociatedPR(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	f.commit(t, "foreign.txt")
	at := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	f.commitPRs = map[string][]string{at: {"other-lane/ex-1-deleted-parent"}}
	_, err := f.repo.newChange(f.ctx, newOptions{issue: "EX-2", title: "feat(rjc): refuse foreign associated work", at: "@-"})
	assertRefusalGate(t, err, "remote-parent")
	detail, ok := errors.AsType[*failure](err)
	if !ok || !strings.Contains(detail.fix, "rjc new --on <bookmark>") {
		t.Fatalf("missing parent remedy: %v", err)
	}
	if f.commitPRCalls[at] != 1 || len(f.pushes) != 0 {
		t.Fatalf("foreign association calls=%v pushes=%v", f.commitPRCalls, f.pushes)
	}
	if got := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id"); got != at {
		t.Fatal("foreign association refusal rewrote work")
	}
}

type failingForgeTransport struct{}

func (failingForgeTransport) RoundTrip(_ *http.Request) (*http.Response, error) {
	return nil, errors.New("fixture network failure")
}

func TestAdoptionRefusesFullAssociationPage(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	f.commit(t, "own.txt")
	at := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	heads := make([]string, 100)
	for i := range heads {
		heads[i] = "test-lane/ex-1-own-former-parent"
	}
	f.commitPRs = map[string][]string{at: heads}
	_, err := f.repo.newChange(f.ctx, newOptions{issue: "EX-2", title: "feat(rjc): refuse incomplete PR history", at: "@-"})
	if err == nil || !strings.Contains(err.Error(), "single-page bound") || f.commitPRCalls[at] != 1 {
		t.Fatalf("full page: err=%v calls=%v", err, f.commitPRCalls)
	}
}
