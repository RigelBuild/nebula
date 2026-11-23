package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestNewAndDryRunSmoke(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	var out, errOut bytes.Buffer
	code := run(f.ctx, []string{"new", "--issue", "EX-1", "--title", "feat(rjc): create safe changes"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("new exit=%d: %s", code, &errOut)
	}
	f.commit(t, "feature.go")
	out.Reset()
	code = run(f.ctx, []string{"submit", "--title", "feat(rjc): create safe changes", "--body-file", f.bodyFile, "--summary", "X " + strings.Repeat("verified behavior ", 20), "--dry-run"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("submit dry-run exit=%d: %s", code, &errOut)
	}
	if len(f.pushes) != 0 || len(f.prs) != 0 {
		t.Fatal("dry-run pushed or wrote a PR")
	}
	if !strings.Contains(out.String(), "test-lane/ex-1-create-safe-changes: prepared") {
		t.Fatalf("resolved bookmark missing from output: %s", &out)
	}
	store := fileBaseline{root: f.repo.stateDir, keys: f.repo.keys}
	path, err := store.path("test-lane/ex-1-create-safe-changes", ".candidate.md")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(body), "## What does this do\n\nThis fixture submits a feature") || !strings.Contains(string(body), "## Summary\n\nX") {
		t.Fatalf("candidate body = %q, err = %v", body, err)
	}
}

func TestNewPRIsCreatedNonDraftByDefault(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): open ready by default", "")
	f.commit(t, "ready-by-default.txt")
	opt := submitOptions{bookmark: bookmark, title: "feat(rjc): open ready by default", bodyFile: f.bodyFile}
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.metadata[f.prs[bookmark]].Draft {
		t.Fatal("new PR is draft, want ready")
	}
}

func TestDraftOptionCreatesDraft(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): keep work in progress", "")
	f.commit(t, "wip.txt")
	opt := submitOptions{bookmark: bookmark, title: "feat(rjc): keep work in progress", bodyFile: f.bodyFile, draft: true}
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !f.metadata[f.prs[bookmark]].Draft {
		t.Fatal("--draft did not create a draft PR")
	}
}

func TestResubmitPreservesDraftState(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		initialDraft  bool
		resubmitDraft bool
	}{
		{name: "ready with draft flag", resubmitDraft: true},
		{name: "draft without draft flag", initialDraft: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := parallelFixtureRepo(t)
			bookmark := f.new(t, "EX-1", "feat(rjc): preserve draft state", "")
			f.commit(t, "state.txt")
			opt := submitOptions{bookmark: bookmark, title: "feat(rjc): preserve draft state", bodyFile: f.bodyFile, draft: tc.initialDraft}
			if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
				t.Fatal(err)
			}
			opt.draft = tc.resubmitDraft
			var log bytes.Buffer
			if err := f.repo.submit(f.ctx, opt, f, &log); err != nil {
				t.Fatal(err)
			}
			if len(f.drafts) != 2 || f.drafts[1] || f.metadata[f.prs[bookmark]].Draft != tc.initialDraft {
				t.Fatalf("engine draft flags %v, PR draft %v", f.drafts, f.metadata[f.prs[bookmark]].Draft)
			}
			if tc.resubmitDraft && (!strings.Contains(log.String(), "--draft applies only at creation") || !strings.Contains(log.String(), "gh pr ready --undo 1 -R example/app") || !strings.Contains(log.String(), "draft: true via GitHub MCP")) {
				t.Fatalf("missing draft notice: %s", &log)
			}
		})
	}
}

func TestSubmitReplacesStaleLedgerPR(t *testing.T) {
	t.Run("new PR", func(t *testing.T) {
		testSubmitReplacesStaleLedgerPR(t, true)
	})
	t.Run("same PR", func(t *testing.T) {
		testSubmitReplacesStaleLedgerPR(t, false)
	})
}

func testSubmitReplacesStaleLedgerPR(t *testing.T, seedForge bool) {
	t.Helper()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): replace stale ledger PR", "")
	f.commit(t, "feature.txt")
	if seedForge {
		// The fixture assigns len(f.prs)+1 to the new PR, so this makes it PR 2.
		f.prs["other-lane/ex-2-existing"] = 1
	}

	created := time.Date(2020, time.January, 2, 3, 4, 5, 0, time.UTC)
	ledger := Ledger{root: f.repo.stateDir}
	old := LedgerEntry{
		Repo:     f.repo.profile.Name,
		PR:       1,
		Bookmark: bookmark,
		Codename: f.repo.codename,
		Created:  created,
	}
	if seedForge {
		if err := ledger.Save(LedgerEntry{Repo: old.Repo, PR: 3, Bookmark: bookmark, Codename: old.Codename}); err != nil {
			t.Fatal(err)
		}
	}
	if err := ledger.Save(old); err != nil {
		t.Fatal(err)
	}

	opt := submitOptions{bookmark: bookmark, title: "feat(rjc): replace stale ledger PR", bodyFile: f.bodyFile}
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}

	wantPR := f.prs[bookmark]
	got, found, err := ledger.Find(old.Repo, bookmark)
	if err != nil || !found || got.PR != wantPR {
		t.Fatalf("ledger entry = %+v, found=%v, err=%v; want PR %d", got, found, err, wantPR)
	}
	if got.Codename != f.repo.codename {
		t.Fatalf("ledger Codename = %q, want %q", got.Codename, f.repo.codename)
	}
	if got.LastPushedHead != f.metadata[wantPR].HeadSHA || got.LastPushedHead == "" {
		t.Fatalf("ledger last pushed head = %q, PR head = %q", got.LastPushedHead, f.metadata[wantPR].HeadSHA)
	}
	if seedForge {
		if !got.Created.After(created) {
			t.Fatalf("replacement Created = %s, want after %s", got.Created, created)
		}
		assertLedgerFilesAbsent(t, ledger, old.Repo, old.PR, 3)
		return
	}
	if !got.Created.Equal(created) {
		t.Fatalf("same-PR ledger Created = %s, want %s", got.Created, created)
	}
}

//nolint:cyclop // One table of adoption scenarios; each case sets up its own jj state before the shared assertions run.
func TestSubmitAdoptsOwnedRemoteBookmark(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		gate string
	}{
		{"unpublished", "adopt-unpublished"},
		{"head mismatch", "adopt-head-mismatch"},
		{"head ref mismatch", "adopt-head-mismatch"},
		{"other writer", "adopt-not-last-pushed"},
		{"legacy ledger", "adopt-no-ledger-head"},
		{"stale ancestor", "adopt-backwards"},
		{"two local targets plus origin", "adopt-ambiguous"},
		{"conflicted rewrite", ""},
		{"conflicted retarget", "retarget-not-owned"},
		{"conflicted foreign", "adopt-foreign-rewrite"},
		{"happy path", ""},
		{"retarget", ""},
		{"dry run", ""},
		{"preflight refusal", "issue-on-existing-pr"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := parallelFixtureRepo(t)
			jjTest(t, f.repo, "config", "set", "--repo", `revset-aliases."immutable_heads()"`, `builtin_immutable_heads() | remote_bookmarks()`)
			bookmark := f.new(t, "EX-1", "feat(rjc): adopt owned remote", "")
			f.commit(t, "published.txt")
			opt := submitOptions{bookmark: bookmark, title: "feat(rjc): adopt owned remote", bodyFile: f.bodyFile}
			if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
				t.Fatal(err)
			}
			published := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark+"@origin"), "-T", "commit_id")
			kept := published
			f.pushes = nil
			jjTest(t, f.repo, "bookmark", "untrack", bookmark+"@origin")
			metadata := f.metadata[f.prs[bookmark]]
			switch tt.name {
			case "unpublished":
				delete(f.prs, bookmark)
			case "head mismatch":
				metadata.HeadSHA = "wrong"
			case "head ref mismatch":
				metadata.HeadRef = "wrong"
			case "legacy ledger":
				ledger := Ledger{root: f.repo.stateDir}
				if err := ledger.recordPush(f.repo.profile.Name, bookmark, f.prs[bookmark], f.repo.codename, ""); err != nil {
					t.Fatal(err)
				}
			case "other writer", "stale ancestor":
				jjTest(t, f.repo, "bookmark", "track", bookmark+"@origin")
				jjTest(t, f.repo, "new", quoteRev(bookmark))
				f.commit(t, "remote.txt")
				jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-", "--allow-backwards")
				jjTest(t, f.repo, "git", "push", "--bookmark", bookmark)
				metadata.HeadSHA = jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark+"@origin"), "-T", "commit_id")
				jjTest(t, f.repo, "bookmark", "untrack", bookmark+"@origin")
				jjTest(t, f.repo, "bookmark", "set", bookmark, "-r", published, "--allow-backwards")
				if tt.name == "stale ancestor" {
					ledger := Ledger{root: f.repo.stateDir}
					if err := ledger.recordPush(f.repo.profile.Name, bookmark, f.prs[bookmark], f.repo.codename, metadata.HeadSHA); err != nil {
						t.Fatal(err)
					}
				}
			case "conflicted rewrite", "conflicted retarget", "conflicted foreign", "two local targets plus origin":
				if tt.name == "conflicted rewrite" || tt.name == "conflicted retarget" {
					// The default immutable_heads() includes untracked remote bookmarks.
					// Rewriting this pushed change requires the fixture-only override.
					jjTest(t, f.repo, "--ignore-immutable", "describe", "-r", quoteRev(bookmark), "-m", "feat(rjc): rewritten change")
					kept = jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
				} else {
					jjTest(t, f.repo, "new", quoteRev(bookmark)+"-")
					f.commit(t, "foreign.txt")
					jjTest(t, f.repo, "bookmark", "set", bookmark, "-r", "@-", "--allow-backwards")
				}
				jjTest(t, f.repo, "bookmark", "track", bookmark+"@origin")
				if tt.name == "conflicted retarget" {
					opt.at = kept
				}
				first := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
				if tt.name == "two local targets plus origin" {
					jjTest(t, f.repo, "new", mainOrigin)
					f.commit(t, "third.txt")
					third := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
					jjTest(t, f.repo, "bookmark", "set", bookmark, "-r", published, "--allow-backwards")
					base := jjTest(t, f.repo, "op", "log", "--no-graph", "--limit", "1", "-T", "id")
					jjTest(t, f.repo, "--at-op", base, "bookmark", "set", bookmark, "-r", first, "--allow-backwards")
					jjTest(t, f.repo, "--at-op", base, "bookmark", "set", bookmark, "-r", third, "--allow-backwards")
					targets := jjTest(t, f.repo, "bookmark", "list", "exact:"+bookmark, "-T", `if(!remote, added_targets.map(|c| c.commit_id()).join("\n"))`)
					if len(strings.Split(targets, "\n")) != 2 {
						t.Fatalf("expected two added targets plus origin, got %q", targets)
					}
				}
			case "dry run":
				opt.dryRun = true
			case "preflight refusal":
				opt.issue = "EX-1"
			case "retarget":
				jjTest(t, f.repo, "new", quoteRev(bookmark))
				f.commit(t, "retarget.txt")
				kept = jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
				opt.at = kept
			}
			if tt.name != "unpublished" {
				f.metadata[f.prs[bookmark]] = metadata
			}
			before := jjTest(t, f.repo, "bookmark", "list", "--all")
			var output bytes.Buffer
			err := f.repo.submit(f.ctx, opt, f, &output)
			if opt.dryRun && (!strings.Contains(output.String(), "would adopt origin") || !strings.Contains(output.String(), "rebase and engine checks run only on real submit")) {
				t.Fatalf("dry run did not report adoption: %s", output.String())
			}
			if tt.gate != "" {
				assertRefusalGate(t, err, tt.gate)
			} else if err != nil {
				t.Fatal(err)
			}
			switch {
			case tt.gate != "" || opt.dryRun:
				after := jjTest(t, f.repo, "bookmark", "list", "--all")
				if before != after || len(f.pushes) != 0 {
					t.Fatalf("bookmark state changed or pushed: before=%s after=%s pushes=%v", before, after, f.pushes)
				}
			case len(f.pushes) != 1:
				t.Fatalf("pushes=%v", f.pushes)
			default:
				local := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
				origin := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark+"@origin"), "-T", "commit_id")
				untracked := jjTest(t, f.repo, "log", "--no-graph", "-r", "untracked_remote_bookmarks(exact:"+quoteRev(bookmark)+", remote=exact:\"origin\")", "-T", "commit_id")
				if local != kept || origin != kept || untracked != "" {
					t.Fatalf("bookmark state: local=%s origin=%s untracked=%s want=%s", local, origin, untracked, kept)
				}
			}
		})
	}
}

func TestAbandonStaleRewritesRejectsThirdCopy(t *testing.T) {
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): protect stale copies", "")
	f.commit(t, "published.txt")
	opt := submitOptions{bookmark: bookmark, title: "feat(rjc): protect stale copies", bodyFile: f.bodyFile}
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	origin := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark+"@origin"), "-T", "commit_id")
	change := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(origin), "-T", "change_id")
	jjTest(t, f.repo, "--ignore-immutable", "describe", "-r", quoteRev(bookmark), "-m", "first rewrite")
	first := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	jjTest(t, f.repo, "bookmark", "create", "other/third", "-r", first)
	jjTest(t, f.repo, "--ignore-immutable", "describe", "-r", quoteRev(origin), "-m", "second rewrite")
	jjTest(t, f.repo, "bookmark", "set", bookmark, "-r", origin, "--allow-backwards")
	copies := jjTest(t, f.repo, "log", "--no-graph", "-r", "change_id("+quoteRev(change)+")", "-T", `commit_id ++ "\n"`)
	if len(strings.Fields(copies)) != 3 {
		t.Fatalf("expected three change copies, got %q", copies)
	}
	err := f.repo.abandonStaleRewrites(f.ctx, []string{origin, first}, []string{"change_id(" + quoteRev(change) + ")"}, map[string]bool{bookmark: true})
	if err == nil || !strings.Contains(err.Error(), "stale rebase left divergent commits") {
		t.Fatalf("expected third divergent copy refusal, got %v", err)
	}
}

func assertLedgerFilesAbsent(t *testing.T, ledger Ledger, repo string, prs ...int) {
	t.Helper()
	for _, pr := range prs {
		path, err := ledger.path(repo, pr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("old ledger file for PR %d stat error = %v, want not-exist", pr, err)
		}
	}
}

func TestSubmitPrunesLeftoverStaleLedgerEntry(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	// Force PR 2 so the stale PR 1 file sorts before the current entry.
	f.prs["other-lane/ex-2-existing"] = 1
	bookmark := f.new(t, "EX-1", "feat(rjc): prune stale ledger entry", "")
	f.commit(t, "first.txt")
	opt := submitOptions{bookmark: bookmark, title: "feat(rjc): prune stale ledger entry", bodyFile: f.bodyFile}
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}

	ledger := Ledger{root: f.repo.stateDir}
	current, found, err := ledger.Find(f.repo.profile.Name, bookmark)
	if err != nil || !found {
		t.Fatalf("initial ledger entry = %+v, found=%v, err=%v", current, found, err)
	}
	stale := LedgerEntry{Repo: current.Repo, PR: current.PR - 1, Bookmark: bookmark, Codename: current.Codename}
	if err := ledger.Save(stale); err != nil {
		t.Fatal(err)
	}

	f.commit(t, "second.txt")
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}

	got, found, err := ledger.Find(current.Repo, bookmark)
	if err != nil || !found || got.PR != current.PR || !got.Created.Equal(current.Created) {
		t.Fatalf("ledger after repeat submit = %+v, found=%v, err=%v; want existing PR %d and Created %s", got, found, err, current.PR, current.Created)
	}
	if got.Codename != current.Codename {
		t.Fatalf("ledger Codename = %q, want %q", got.Codename, current.Codename)
	}
	stalePath, err := ledger.path(stale.Repo, stale.PR)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stalePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale ledger file stat error = %v, want not-exist", err)
	}
}
func TestAdoptionSubmitCommitsBaseline(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	f.commit(t, "feature.go")
	err := f.repo.submit(f.ctx, submitOptions{issue: "EX-1", title: "feat: adopt safe changes", bodyFile: f.bodyFile}, f, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	bookmark := f.repo.submittedBookmark
	baseline, exists, err := (fileBaseline{root: f.repo.stateDir, keys: f.repo.keys}).Load(bookmark)
	if err != nil || !exists || baseline.Authored == "" || len(f.pushes) != 1 {
		t.Fatalf("adoption baseline = %+v, exists=%v, pushes=%v, err=%v", baseline, exists, f.pushes, err)
	}
}

func TestMultipleIssuePersistence(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	var out, errOut bytes.Buffer
	code := run(f.ctx, []string{"new", "--issue", "EX-1", "--issue", "EX-2", "--title", "feat(rjc): create linked changes"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("new exit=%d: %s", code, &errOut)
	}
	bookmark := "test-lane/ex-1-create-linked-changes"
	f.commit(t, "linked.go")
	code = run(f.ctx, []string{"meta", bookmark, "--title", "feat(rjc): create linked changes", "--body-file", f.bodyFile}, &out, &errOut)
	if code != 0 {
		t.Fatalf("meta exit=%d: %s", code, &errOut)
	}
	issues, err := f.repo.loadIssues(bookmark)
	if err != nil || len(issues) != 2 || issues[0] != 1 || issues[1] != 2 {
		t.Fatalf("issues = %v, %v", issues, err)
	}
	c, err := f.repo.loadCandidate(bookmark)
	if err != nil || c.title != "feat(rjc): create linked changes (EX-1, EX-2)" || !strings.Contains(c.body, "Refs EX-1\nRefs EX-2\n") {
		t.Fatalf("candidate = %+v, %v", c, err)
	}
	code = run(f.ctx, []string{"submit", "--title", "feat(rjc): create linked changes", "--body-file", f.bodyFile, "--dry-run"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("submit exit=%d: %s", code, &errOut)
	}
}

func TestUnbookmarkedMultipleIssuesBoundsBeforeMutation(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	f.commit(t, "feature.go")
	before := jjTest(t, f.repo, "log", "--no-graph", "-r", "all()", "-T", "commit_id ++ change_id ++ bookmarks ++ description")
	var out bytes.Buffer
	title := "feat: " + strings.Repeat("safe ", 15)
	code := run(f.ctx, []string{"submit", "--issue", "EX-1", "--issue", "EX-2", "--title", title, "--body-file", f.bodyFile}, &out, &out)
	if code != 1 || !strings.Contains(out.String(), "metadata-bounds") {
		t.Fatalf("exit=%d output=%s", code, &out)
	}
	after := jjTest(t, f.repo, "log", "--no-graph", "-r", "all()", "-T", "commit_id ++ change_id ++ bookmarks ++ description")
	if before != after {
		t.Fatal("refusal changed history")
	}
}

func TestSubmitOutsideLineLeavesHistory(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): protect local main", "")
	f.commit(t, "main-ahead.txt")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	f.commit(t, "owned.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	f.commit(t, "unbookmarked.txt")
	before := jjTest(t, f.repo, "log", "--no-graph", "-r", "all()", "-T", "commit_id ++ change_id ++ bookmarks ++ description")
	var out bytes.Buffer
	code := run(f.ctx, []string{"submit", bookmark, "--title", "feat(rjc): protect local main", "--body-file", f.bodyFile}, &out, &out)
	if code != 1 || !strings.Contains(out.String(), "outside-line") {
		t.Fatalf("exit=%d output=%s", code, &out)
	}
	after := jjTest(t, f.repo, "log", "--no-graph", "-r", "all()", "-T", "commit_id ++ change_id ++ bookmarks ++ description")
	if before != after {
		t.Fatal("refusal changed history or bookmarks")
	}
}

//nolint:tparallel // Subtests share the parent fixture.
func TestLowercaseAuthoredIssueRefused(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	for _, args := range [][]string{
		{"new", "--issue", "EX-1", "--title", "feat(rjc): linked change (ex-123)"},
		{"meta", "test-lane/ex-1-linked-change", "--title", "feat(rjc): linked change (ex-123)", "--body-file", f.bodyFile},
	} {
		t.Run(args[0], func(t *testing.T) {
			var out bytes.Buffer
			if code := run(f.ctx, args, &out, &out); code != 1 || !strings.Contains(out.String(), "authored title contains an issue reference") {
				t.Fatalf("exit=%d output=%s", code, &out)
			}
		})
	}
}

func TestDuplicateIssuesRefused(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	var out bytes.Buffer
	code := run(f.ctx, []string{"new", "--issue", "EX-1", "--issue", "EX-1", "--title", "feat(rjc): linked change"}, &out, &out)
	if code != 1 || !strings.Contains(out.String(), "duplicate-issue") {
		t.Fatalf("exit=%d output=%s", code, &out)
	}
}

func TestSubmitIssueMismatchOnUnpublishedBookmark(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): linked change", "")
	f.commit(t, "linked.txt")
	var out bytes.Buffer
	code := run(f.ctx, []string{"submit", bookmark, "--issue", "EX-2", "--title", "feat(rjc): linked change", "--body-file", f.bodyFile, "--dry-run"}, &out, &out)
	if code != 1 || !strings.Contains(out.String(), "issue-mismatch") {
		t.Fatalf("exit=%d output=%s", code, &out)
	}
	out.Reset()
	code = run(f.ctx, []string{"submit", bookmark, "--issue", "EX-1", "--title", "feat(rjc): linked change", "--body-file", f.bodyFile, "--dry-run"}, &out, &out)
	if code != 0 {
		t.Fatalf("matching issue exit=%d output=%s", code, &out)
	}
}

func TestSubmitRejectsOtherWorkspaceLine(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): add isolated change", "")
	f.commit(t, "isolated.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	jjTest(t, f.repo, "new", mainOrigin)
	err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark}, f, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("expected wrong-workspace refusal, got %v", err)
	}
}

func TestStackSavedCandidates(t *testing.T) {
	t.Parallel()
	for _, saved := range []bool{false, true} {
		t.Run(map[bool]string{false: "new without meta refuses", true: "saved candidates submit bottom up"}[saved], func(t *testing.T) {
			t.Parallel()
			f := parallelFixtureRepo(t)
			lower := f.new(t, "EX-1", "feat(rjc): add lower change", "")
			f.commit(t, "lower.txt")
			jjTest(t, f.repo, "bookmark", "move", lower, "--to", "@-")
			upper, err := f.repo.newChange(f.ctx, newOptions{issue: "EX-2", title: "feat(rjc): add upper change", on: lower})
			if err != nil {
				t.Fatal(err)
			}
			f.commit(t, "upper.txt")
			if saved {
				var out bytes.Buffer
				if code := run(f.ctx, []string{"meta", lower, "--title", "feat(rjc): add lower change", "--body-file", f.bodyFile}, &out, &out); code != 0 {
					t.Fatalf("meta exit=%d: %s", code, &out)
				}
			}
			before := jjTest(t, f.repo, "log", "--no-graph", "-r", "all()", "-T", "commit_id ++ change_id ++ bookmarks ++ description")
			err = f.repo.submit(f.ctx, submitOptions{bookmark: upper, title: "feat(rjc): add upper change (EX-2)", bodyFile: f.bodyFile}, f, io.Discard)
			if !saved {
				var detail *failure
				if !errors.As(err, &detail) || !strings.Contains(err.Error(), lower) || !strings.Contains(detail.fix, "rjc meta "+lower) || len(f.pushes) != 0 {
					t.Fatalf("expected pre-push lower-candidate refusal, got %v pushes=%v", err, f.pushes)
				}
				after := jjTest(t, f.repo, "log", "--no-graph", "-r", "all()", "-T", "commit_id ++ change_id ++ bookmarks ++ description")
				if before != after {
					t.Fatal("lower candidate refusal changed history")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(f.pushes, ",") != lower+","+upper {
				t.Fatalf("push order = %v", f.pushes)
			}
			if got := f.metadata[f.prs[lower]].Title; got != "feat(rjc): add lower change (EX-1)" {
				t.Fatalf("target title overwrote saved lower title: %s", got)
			}
			checkRepeatedStackSkips(t, f, lower, upper)
		})
	}
}

func TestSubmitSelectedOwnedStackParent(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	parent := f.new(t, "EX-1", "feat(rjc): add parent", "")
	f.commit(t, "parent.txt")
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
	child := f.new(t, "EX-2", "feat(rjc): add child", parent)
	f.commit(t, "child.txt")
	var out bytes.Buffer
	if code := run(f.ctx, []string{"meta", parent, "--title", "feat(rjc): add parent", "--body-file", f.bodyFile}, &out, &out); code != 0 {
		t.Fatalf("meta exit=%d: %s", code, &out)
	}
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: child, title: "feat(rjc): add child (EX-2)", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	parentTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(parent), "-T", "commit_id")
	childParent := jjTest(t, f.repo, "log", "--no-graph", "-r", "roots("+quoteRev(parent)+".."+quoteRev(child)+")-", "-T", "commit_id")
	metadata := f.metadata[f.prs[child]]
	if childParent != parentTip || metadata.BaseRef != parent || metadata.BaseSHA != parentTip {
		t.Fatalf("parent=%s child parent=%s metadata=%+v", parentTip, childParent, metadata)
	}
	if strings.Join(f.pushes, ",") != parent+","+child {
		t.Fatalf("pushes = %v", f.pushes)
	}
}

func checkRepeatedStackSkips(t *testing.T, f *fixture, lower, upper string) {
	t.Helper()
	for _, reason := range []string{""} {
		opt := submitOptions{bookmark: upper, title: "feat(rjc): add upper change (EX-2)", bodyFile: f.bodyFile, skip: reason}
		if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
			t.Fatal(err)
		}
		for _, bookmark := range []string{lower, upper} {
			body := f.metadata[f.prs[bookmark]].Body
			if reason == "" {
				if strings.Contains(body, "Checks skipped:") {
					t.Fatalf("normal submit retained a skip annotation for %s: %s", bookmark, body)
				}
				continue
			}
			if strings.Count(body, "Checks skipped:") != 1 || !strings.Contains(body, "Checks skipped: "+reason) {
				t.Fatalf("stack annotation for %s: %s", bookmark, body)
			}
		}
	}
}

func TestMetaBoundsAndNewDoesNotSave(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): save metadata safely", "")
	if _, err := f.repo.loadCandidate(bookmark); err == nil {
		t.Fatal("new saved a metadata candidate")
	}
	writeTestFile(t, f.bodyFile, "## What does this do\n\nShort.\n\n## Why do we need it\n\nA test need.\n\n## Summary\n\nShort.\n\n## Verification\n\nShort.\n\n## Risks\n\nShort.\n")
	var out bytes.Buffer
	if code := run(f.ctx, []string{"meta", bookmark, "--title", "feat(rjc): save metadata safely", "--body-file", f.bodyFile}, &out, &out); code != 1 {
		t.Fatalf("meta bounds exit=%d: %s", code, &out)
	}
	if _, err := f.repo.loadCandidate(bookmark); err == nil {
		t.Fatal("bounds failure saved a candidate")
	}
}

func TestFailedPushKeepsPublishedBaseline(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): save safe changes", "")
	f.commit(t, "feature.txt")
	opt := submitOptions{bookmark: bookmark, title: "feat(rjc): save safe changes (EX-1)", bodyFile: f.bodyFile}
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	store := fileBaseline{root: f.repo.stateDir, keys: f.repo.keys}
	baseline, exists, err := store.Load(bookmark)
	if err != nil || !exists {
		t.Fatalf("initial baseline missing: %v", err)
	}
	body, err := os.ReadFile(f.bodyFile)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, f.bodyFile, strings.Replace(string(body), "safe submission words", "new candidate words", 1))
	f.commit(t, "second.txt")
	f.pushError = errors.New("fixture push failure")
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err == nil {
		t.Fatal("failed push accepted")
	}
	after, _, err := store.Load(bookmark)
	if err != nil || after != baseline {
		t.Fatalf("failed push changed baseline: %+v %v", after, err)
	}
	remote := f.metadata[f.prs[bookmark]]
	original := remote
	remote.Body = "human edit\n"
	f.metadata[f.prs[bookmark]] = remote
	pushes := len(f.pushes)
	err = f.repo.submit(f.ctx, opt, f, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--- published\n"+baseline.Authored) || len(f.pushes) != pushes {
		t.Fatalf("next run did not compare against A: %v", err)
	}
	f.metadata[f.prs[bookmark]] = original
	f.pushError = nil
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	after, _, err = store.Load(bookmark)
	if err != nil || after == baseline {
		t.Fatalf("confirmed retry did not publish B: %+v %v", after, err)
	}
}

func TestCoveredPushStillRequestsReviewWhenReconcileRefuses(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): preserve human metadata", "")
	f.commit(t, "first.txt")
	opt := submitOptions{bookmark: bookmark, title: "feat(rjc): preserve human metadata", bodyFile: f.bodyFile}
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	baseline, _, err := (fileBaseline{root: f.repo.stateDir, keys: f.repo.keys}).Load(bookmark)
	if err != nil {
		t.Fatal(err)
	}
	f.timeline = []map[string]any{{"event": "reviewed", "state": "approved", "id": 11}}
	f.commit(t, "second.txt")
	f.afterPush = func() {
		remote := f.metadata[f.prs[bookmark]]
		remote.Body = "human edit during push\n"
		f.metadata[f.prs[bookmark]] = remote
	}
	var out bytes.Buffer
	code := run(f.ctx, []string{"submit", bookmark, "--title", opt.title, "--body-file", f.bodyFile}, &out, &out)
	if code != 1 || !strings.Contains(out.String(), "human-body-edit") {
		t.Fatalf("expected reconciliation refusal, exit=%d: %s", code, &out)
	}
	head := f.metadata[f.prs[bookmark]].HeadSHA
	if len(f.reviewCalls) != 2 || !strings.Contains(f.reviewCalls[1], head) {
		t.Fatalf("review handoff missing new head: %v", f.reviewCalls)
	}
	after, _, err := (fileBaseline{root: f.repo.stateDir, keys: f.repo.keys}).Load(bookmark)
	if err != nil || after != baseline {
		t.Fatalf("human edit advanced baseline: %+v %v", after, err)
	}
}

// holdBodies returns the fixture comments whose body starts with a Hold line.
func holdBodies(comments []map[string]any) []string {
	var holds []string
	for _, comment := range comments {
		if body, _ := comment["body"].(string); strings.HasPrefix(body, "Hold:") {
			holds = append(holds, body)
		}
	}
	return holds
}

func TestHoldCommentExistsWhenSubmitReturns(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-10694", "feat(rjc): post a Hold comment from submit", "")
	f.commit(t, "hold.txt")
	opt := submitOptions{bookmark: bookmark, title: "feat(rjc): post a Hold comment from submit", bodyFile: f.bodyFile, hold: "review pending"}
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := holdBodies(f.comments); len(got) != 1 || got[0] != "Hold: review pending" {
		t.Fatalf("Hold comments when submit returned = %q, want one Hold: review pending", got)
	}
}

func TestHoldPostedWhenHeadWaitFails(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-10694", "feat(rjc): post Hold before waiting", "")
	f.commit(t, "hold-order.txt")
	stop := errors.New("stop before head confirmation")
	f.repo.pushedHeadWait = func(context.Context, time.Duration) error { return stop }
	f.afterPush = func() {
		remote := f.metadata[f.prs[bookmark]]
		remote.HeadSHA = strings.Repeat("f", 40)
		f.metadata[f.prs[bookmark]] = remote
	}
	opt := submitOptions{bookmark: bookmark, title: "feat(rjc): post Hold before waiting", bodyFile: f.bodyFile, hold: "review pending"}
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); !errors.Is(err, stop) {
		t.Fatalf("submit error = %v, want the head wait to fail", err)
	}
	if got := holdBodies(f.comments); len(got) != 1 {
		t.Fatalf("Hold comments after head wait failed = %q, want the Hold posted first", got)
	}
}

func TestHoldPostFailureFailsSubmitNamingPR(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-10694", "feat(rjc): fail loudly without a Hold", "")
	f.commit(t, "hold-fail.txt")
	f.holdStatus = http.StatusInternalServerError
	opt := submitOptions{bookmark: bookmark, title: "feat(rjc): fail loudly without a Hold", bodyFile: f.bodyFile, hold: "review pending"}
	err := f.repo.submit(f.ctx, opt, f, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "PR 1") || !strings.Contains(err.Error(), "Hold comment") {
		t.Fatalf("submit error = %v, want a failure naming PR 1 and the Hold comment", err)
	}
	if got := holdBodies(f.comments); len(got) != 0 {
		t.Fatalf("failed Hold post left comments %q", got)
	}
}

func TestHoldIsIdempotentUntilAllClear(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-10694", "feat(rjc): keep one Hold per reason", "")
	f.commit(t, "hold-idem.txt")
	opt := submitOptions{bookmark: bookmark, title: "feat(rjc): keep one Hold per reason", bodyFile: f.bodyFile, hold: "review pending"}
	for range 2 {
		if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	if got := holdBodies(f.comments); len(got) != 1 {
		t.Fatalf("Hold comments after resubmit = %q, want one", got)
	}
	allClear := "<!-- rjc:all-clear reviewed=" + strings.Repeat("a", 40) + " base=main -->"
	f.comments = append(f.comments, map[string]any{"id": len(f.comments) + 1, "body": allClear, "user": map[string]string{"login": "example-agent"}})
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := holdBodies(f.comments); len(got) != 2 {
		t.Fatalf("Hold comments after all-clear = %q, want a new Hold", got)
	}
}

func TestHoldFlagPostsThroughCLI(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-10694", "feat(rjc): post a Hold comment from submit", "")
	f.commit(t, "hold-cli.txt")
	var out bytes.Buffer
	code := run(f.ctx, []string{"submit", bookmark, "--title", "feat(rjc): post a Hold comment from submit", "--body-file", f.bodyFile, "--hold", "review pending"}, &out, &out)
	if code != 0 || len(holdBodies(f.comments)) != 1 {
		t.Fatalf("exit=%d holds=%q: %s", code, holdBodies(f.comments), &out)
	}
}

func TestHoldFlagFailureExitsNonZero(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-10694", "feat(rjc): fail loudly from the CLI", "")
	f.commit(t, "hold-cli-fail.txt")
	f.holdStatus = http.StatusInternalServerError
	var out bytes.Buffer
	code := run(f.ctx, []string{"submit", bookmark, "--title", "feat(rjc): fail loudly from the CLI", "--body-file", f.bodyFile, "--hold", "review pending"}, &out, &out)
	if code == 0 || !strings.Contains(out.String(), "PR 1") {
		t.Fatalf("exit=%d, want non-zero naming PR 1: %s", code, &out)
	}
}

func TestHoldBlankReasonRefusedBeforeWrites(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-10694", "feat(rjc): refuse a blank Hold", "")
	f.commit(t, "hold-blank.txt")
	var out bytes.Buffer
	code := run(f.ctx, []string{"submit", bookmark, "--title", "feat(rjc): refuse a blank Hold", "--body-file", f.bodyFile, "--hold", "   "}, &out, &out)
	if code != 1 || len(f.pushes) != 0 || len(f.prs) != 0 {
		t.Fatalf("exit=%d pushes=%v prs=%v: %s", code, f.pushes, f.prs, &out)
	}
}

// lastCommentBody returns the newest fixture comment body.
func lastCommentBody(t *testing.T, comments []map[string]any) string {
	t.Helper()
	if len(comments) == 0 {
		t.Fatal("no comments posted")
	}
	body, _ := comments[len(comments)-1]["body"].(string)
	return body
}

func TestHoldPostedAfterHeadMovedComment(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-10694", "feat(rjc): hold after head moved", "")
	f.commit(t, "moved-1.txt")
	opt := submitOptions{bookmark: bookmark, title: "feat(rjc): hold after head moved", bodyFile: f.bodyFile, hold: "review pending"}
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	// An all-clear lifts the first Hold, so the resubmit must repost it after the head-moved comment.
	allClear := "<!-- rjc:all-clear reviewed=" + strings.Repeat("a", 40) + " base=main -->"
	f.comments = append(f.comments, map[string]any{"id": len(f.comments) + 1, "body": allClear, "user": map[string]string{"login": "example-agent"}})
	f.timeline = []map[string]any{{"event": "reviewed", "state": "approved", "id": 11}}
	f.commit(t, "moved-2.txt")
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	moved := false
	for _, c := range f.comments {
		if body, _ := c["body"].(string); strings.HasPrefix(body, "Head moved") {
			moved = true
		}
	}
	if !moved || lastCommentBody(t, f.comments) != "Hold: review pending" {
		t.Fatalf("comments %v, want Head moved then Hold: review pending last", f.comments)
	}
}

func TestHoldPostedAfterRetargetComment(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-10694", "feat(rjc): hold after retarget", "")
	jjTest(t, f.repo, "git", "push", "--bookmark", bookmark)
	old := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	f.prs[bookmark] = 1
	f.metadata[1] = Metadata{HeadRef: bookmark, HeadSHA: old, BaseRef: f.repo.profile.Trunk}
	f.commit(t, "retarget.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	item := preparedChange{change: Change{Bookmark: bookmark, Tip: tip, Base: mainOrigin}, pr: 1, retarget: true, before: f.metadata[1]}
	item.state.Coverage = Covered
	// The bare fixture PR has no published baseline, so reconcile refuses; the deferred comments still run, as in TestRetargetAlreadyOnTrunkRequestsReview.
	err := f.repo.pushChange(f.ctx, &item, submitOptions{hold: "review pending"}, f, io.Discard)
	if len(f.comments) < 2 || !strings.HasPrefix(f.comments[0]["body"].(string), "rjc retargeted") || lastCommentBody(t, f.comments) != "Hold: review pending" {
		t.Fatalf("comments %v (err %v), want the retarget notice then Hold: review pending last", f.comments, err)
	}
}

func TestHoldStaysAfterLaterViewerComment(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-10694", "feat(rjc): keep a Hold in force", "")
	f.commit(t, "in-force.txt")
	opt := submitOptions{bookmark: bookmark, title: "feat(rjc): keep a Hold in force", bodyFile: f.bodyFile, hold: "review pending"}
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	f.comments = append(f.comments, map[string]any{"id": len(f.comments) + 1, "body": "Updating: fixing review findings", "user": map[string]string{"login": "example-agent"}})
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := holdBodies(f.comments); len(got) != 1 {
		t.Fatalf("Hold comments %q, want one: a non-lifting comment leaves the Hold in force", got)
	}
}

func TestHoldInForceMatchesNemoMerge(t *testing.T) {
	const viewer = "example-agent"
	mine := func(body string) issueComment {
		c := issueComment{Body: body}
		c.User.Login = viewer
		return c
	}
	other := issueComment{Body: "Hold lifted"}
	other.User.Login = "someone-else"
	for _, tc := range []struct {
		name     string
		comments []issueComment
		want     bool
	}{
		{"none", nil, false},
		{"same reason", []issueComment{mine("Hold: review pending")}, true},
		{"reason in a later line", []issueComment{mine("Notes\n\nhold: review pending ")}, true},
		{"different reason", []issueComment{mine("Hold: waiting on review")}, false},
		{"later plain comment", []issueComment{mine("Hold: review pending"), mine("Updating: fixing findings")}, true},
		{"later lift line", []issueComment{mine("Hold: review pending"), mine("Hold lifted: review done")}, false},
		{"later unhold", []issueComment{mine("Hold: review pending"), mine("unhold")}, false},
		{"later all-clear line", []issueComment{mine("Hold: review pending"), mine("All-clear at `1bf13a4df834`.")}, false},
		{"later all-clear marker", []issueComment{mine("Hold: review pending"), mine("<!-- rjc:all-clear reviewed=" + strings.Repeat("a", 40) + " base=main -->")}, false},
		{"other login lifts", []issueComment{mine("Hold: review pending"), other}, true},
		{"quoted hold", []issueComment{mine("> Hold: review pending")}, false},
		{"fenced hold", []issueComment{mine("```\nHold: review pending\n```")}, false},
		{"bold hold line", []issueComment{mine("**Hold:** review pending")}, true},
		{"bold lift", []issueComment{mine("Hold: review pending"), mine("**Hold lifted**")}, false},
		{"bullet lift", []issueComment{mine("Hold: review pending"), mine("- Hold lifted")}, false},
		{"heading lift", []issueComment{mine("Hold: review pending"), mine("## Hold lifted")}, false},
		{"numbered unhold", []issueComment{mine("Hold: review pending"), mine("1. unhold")}, false},
		{"inline code then lift", []issueComment{mine("Hold: review pending"), mine("`x` Hold lifted")}, false},
		{"underscore lift", []issueComment{mine("Hold: review pending"), mine("Hold_lifted")}, false},
		{"zero-width lift", []issueComment{mine("Hold: review pending"), mine("\u200bHold lifted")}, false},
		{"bold all-clear", []issueComment{mine("Hold: review pending"), mine("**All-clear at 1bf13a4d.**")}, false},
		{"italic all-clear", []issueComment{mine("Hold: review pending"), mine("*All-clear at 1bf13a4d*")}, false},
		{"nbsp lift", []issueComment{mine("Hold: review pending"), mine("Hold\u00a0lifted")}, false},
		{"nbsp entity lift", []issueComment{mine("Hold: review pending"), mine("Hold&nbsp;lifted")}, false},
		{"em-space lift", []issueComment{mine("Hold: review pending"), mine("Hold\u2003lifted")}, false},
		{"vtab lift", []issueComment{mine("Hold: review pending"), mine("Hold\vlifted")}, false},
		{"nbsp bullet lift", []issueComment{mine("Hold: review pending"), mine("-\u00a0Hold lifted")}, false},
		{"arabic digit list unhold", []issueComment{mine("Hold: review pending"), mine("\u0661. unhold")}, false},
		{"nbsp heading all-clear", []issueComment{mine("Hold: review pending"), mine("#\u00a0All-clear at 1bf13a4d.")}, false},
		{"dotless i lift", []issueComment{mine("Hold: review pending"), mine("Hold l\u0131fted")}, false},
		{"dotted I lift", []issueComment{mine("Hold: review pending"), mine("HOLD L\u0130FTED")}, false},
		{"nbsp fence hides own Hold", []issueComment{mine("\u00a0```\nHold: review pending")}, false},
		{"nbsp entity fence", []issueComment{mine("&nbsp;```\nHold: review pending")}, false},
		{"ideographic-space fence", []issueComment{mine("\u3000~~~\nHold: review pending")}, false},
		{"fence after U+2028", []issueComment{mine("x\u2028```\nHold: review pending")}, false},
		{"fence after NEL", []issueComment{mine("x\u0085```\nHold: review pending")}, false},
		{"fence after CR", []issueComment{mine("x\r```\nHold: review pending")}, false},
		{"strike across U+2028", []issueComment{mine("~~a\u2028b~~Hold: review pending")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := holdInForce(tc.comments, viewer, "review pending"); got != tc.want {
				t.Fatalf("holdInForce = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHoldMarkReadyFailureNamesDraftPR(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-10694", "feat(rjc): name a held draft", "")
	f.commit(t, "ready-fail.txt")
	f.readyStatus = http.StatusInternalServerError
	opt := submitOptions{bookmark: bookmark, title: "feat(rjc): name a held draft", bodyFile: f.bodyFile, hold: "review pending"}
	err := f.repo.submit(f.ctx, opt, f, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "PR 1 has its Hold but is still draft") {
		t.Fatalf("submit error = %v, want PR 1 named as held but draft", err)
	}
	detail, ok := errors.AsType[*failure](err)
	if !ok || detail.fix != "run gh pr ready 1 -R example/app" || len(holdBodies(f.comments)) != 1 {
		t.Fatalf("failure = %+v holds=%q", detail, holdBodies(f.comments))
	}
}

func TestHoldPostedOnEveryStackPR(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	parent := f.new(t, "EX-1", "feat(rjc): add held parent", "")
	f.commit(t, "held-parent.txt")
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
	child := f.new(t, "EX-2", "feat(rjc): add held child", parent)
	f.commit(t, "held-child.txt")
	var out bytes.Buffer
	if code := run(f.ctx, []string{"meta", parent, "--title", "feat(rjc): add held parent", "--body-file", f.bodyFile}, &out, &out); code != 0 {
		t.Fatalf("meta exit=%d: %s", code, &out)
	}
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: child, title: "feat(rjc): add held child (EX-2)", bodyFile: f.bodyFile, hold: "review pending"}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, bookmark := range []string{parent, child} {
		pr := f.prs[bookmark]
		if got := f.prCommentBodies(pr); len(got) == 0 || got[len(got)-1] != "Hold: review pending" {
			t.Fatalf("PR %d (%s) comments %q, want its Hold last", pr, bookmark, got)
		}
	}
}

func TestHoldChangedReasonPostsNewHold(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-10694", "feat(rjc): new reason new Hold", "")
	f.commit(t, "reason.txt")
	opt := submitOptions{bookmark: bookmark, title: "feat(rjc): new reason new Hold", bodyFile: f.bodyFile, hold: "review pending"}
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	opt.hold = "waiting on review"
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := holdBodies(f.comments); len(got) != 2 || got[1] != "Hold: waiting on review" {
		t.Fatalf("Hold comments %q, want a new Hold for the changed reason", got)
	}
}

func TestHoldNewPRCreatedDraftThenReady(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-10694", "feat(rjc): draft until held", "")
	f.commit(t, "draft-hold.txt")
	opt := submitOptions{bookmark: bookmark, title: "feat(rjc): draft until held", bodyFile: f.bodyFile, hold: "review pending"}
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	pr := f.prs[bookmark]
	if len(f.drafts) != 1 || !f.drafts[0] || len(f.readyCalls) != 1 || f.readyCalls[0] != pr || f.metadata[pr].Draft || len(holdBodies(f.comments)) != 1 {
		t.Fatalf("drafts=%v ready=%v draft=%v holds=%q, want draft creation, Hold, then ready", f.drafts, f.readyCalls, f.metadata[pr].Draft, holdBodies(f.comments))
	}
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(f.drafts) != 2 || f.drafts[1] || len(f.readyCalls) != 1 {
		t.Fatalf("resubmit drafts=%v ready=%v, want an existing PR left as is", f.drafts, f.readyCalls)
	}
}

func TestHoldExistingDraftStaysDraft(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-10694", "feat(rjc): keep an existing draft", "")
	f.commit(t, "keep-draft.txt")
	opt := submitOptions{bookmark: bookmark, title: "feat(rjc): keep an existing draft", bodyFile: f.bodyFile, draft: true}
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	opt.draft, opt.hold = false, "review pending"
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !f.metadata[f.prs[bookmark]].Draft || len(f.readyCalls) != 0 || len(holdBodies(f.comments)) != 1 {
		t.Fatalf("draft=%v ready=%v holds=%q, want the draft kept with a Hold", f.metadata[f.prs[bookmark]].Draft, f.readyCalls, holdBodies(f.comments))
	}
}

func TestHoldPostFailureLeavesNewPRDraft(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-10694", "feat(rjc): stay draft without a Hold", "")
	f.commit(t, "draft-fail.txt")
	f.holdStatus = http.StatusInternalServerError
	opt := submitOptions{bookmark: bookmark, title: "feat(rjc): stay draft without a Hold", bodyFile: f.bodyFile, hold: "review pending"}
	err := f.repo.submit(f.ctx, opt, f, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "PR 1") || !strings.Contains(err.Error(), "left draft") {
		t.Fatalf("submit error = %v, want a failure naming PR 1 left draft", err)
	}
	if !f.metadata[1].Draft || len(f.readyCalls) != 0 {
		t.Fatalf("draft=%v ready=%v, want the PR left draft", f.metadata[1].Draft, f.readyCalls)
	}
}
