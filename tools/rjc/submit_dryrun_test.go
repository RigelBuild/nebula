package main

import (
	"bytes"
	"io"
	"testing"
)

func TestSubmitRetargetDryRunLeavesBookmark(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		onOldTip bool
	}{
		{name: "child-of-old-tip", onOldTip: true},
		{name: "sibling-on-main", onOldTip: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := parallelFixtureRepo(t)
			bookmark := f.new(t, "EX-1", "feat(rjc): rebuild submitted change", "")
			f.commit(t, "rebuilt.txt")
			jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
			opt := submitOptions{bookmark: bookmark, title: "feat(rjc): rebuild submitted change", bodyFile: f.bodyFile}
			if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
				t.Fatal(err)
			}
			oldTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
			rebuildOn := quoteRev(mainOrigin)
			if tc.onOldTip {
				rebuildOn = quoteRev(oldTip)
			}
			jjTest(t, f.repo, "new", rebuildOn)
			f.commit(t, "rebuilt-new.txt")
			newTip := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
			before := jjTest(t, f.repo, "log", "--no-graph", "-r", "all()", "-T", "commit_id ++ bookmarks")
			opt.at, opt.dryRun = newTip, true
			if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
				t.Fatal(err)
			}
			got := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
			after := jjTest(t, f.repo, "log", "--no-graph", "-r", "all()", "-T", "commit_id ++ bookmarks")
			if got != oldTip || before != after || len(f.pushes) != 1 {
				t.Fatalf("dry-run changed bookmark/history or pushed: tip=%s before=%s after=%s pushes=%v", got, before, after, f.pushes)
			}
		})
	}
}

func TestSubmitDryRunLeavesStoreUnchangedWhenLowerMoved(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	lower := f.new(t, "EX-1", "feat(rjc): lower line", "")
	f.commit(t, "lower-old.txt")
	jjTest(t, f.repo, "bookmark", "move", lower, "--to", "@-")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: lower, title: "feat(rjc): lower line", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	upper := f.new(t, "EX-2", "feat(rjc): upper line", lower)
	f.commit(t, "upper-old.txt")
	jjTest(t, f.repo, "bookmark", "move", upper, "--to", "@-")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: upper, title: "feat(rjc): upper line", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}

	// The lower PR moves to a new tip on main. Its bookmark is not pushed.
	jjTest(t, f.repo, "new", mainOrigin)
	f.commit(t, "lower-new.txt")
	newLower := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	jjTest(t, f.repo, "bookmark", "move", lower, "--to", newLower, "--allow-backwards")
	// The rebuilt upper tip sits on the new lower tip.
	jjTest(t, f.repo, "new", quoteRev(newLower))
	f.commit(t, "upper-new.txt")
	newUpper := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")

	before := opLogID(t, f)
	bookmarksBefore := jjTest(t, f.repo, "bookmark", "list", "--all")
	pushes := len(f.pushes)
	err := f.repo.submit(f.ctx, submitOptions{bookmark: upper, at: newUpper, title: "feat(rjc): upper line", bodyFile: f.bodyFile, dryRun: true}, f, io.Discard)
	if err != nil {
		t.Fatalf("dry run refused a valid planned line: %v", err)
	}
	if after := opLogID(t, f); after != before {
		ops := jjTest(t, f.repo, "op", "log", "--no-graph", "--limit", "8", "-T", `description ++ "\n"`)
		t.Fatalf("dry run changed the op log: before=%s after=%s\nrecent ops:\n%s", before, after, ops)
	}
	if after := jjTest(t, f.repo, "bookmark", "list", "--all"); after != bookmarksBefore {
		t.Fatalf("dry run changed bookmarks:\nbefore:\n%s\nafter:\n%s", bookmarksBefore, after)
	}
	if len(f.pushes) != pushes {
		t.Fatalf("dry run pushed: %v", f.pushes)
	}
}

// TestSubmitDryRunFetchesWhenOriginMoved pins why the dry run keeps its fetch.
// The preflight reads origin's refs, so a dry run that skipped the fetch would
// judge the line against a stale origin. The fetch fast-forwards any tracked
// local bookmark that origin moved, so a local main moves too. The dry run
// itself moves no owned bookmark and rewrites no commit.
func TestSubmitDryRunFetchesWhenOriginMoved(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): origin moved line", "")
	f.commit(t, "origin-moved.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: "feat(rjc): origin moved line", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}

	// Another clone advances main on origin. This clone has not fetched it yet.
	other := cloneFixtureRepo(t, f.repo, "other-origin-moved")
	jjTest(t, other, "bookmark", "track", mainOrigin)
	jjTest(t, other, "new", mainOrigin)
	writeTestFile(t, other.root+"/trunk-moved.txt", "moved on origin\n")
	jjTest(t, other, "commit", "-m", "fix: trunk moved on origin")
	jjTest(t, other, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, other, "git", "push", "--bookmark", "main")

	ownedLocal := func() string {
		return jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", `commit_id ++ " " ++ bookmarks.join(",")`)
	}
	ownedBefore := ownedLocal()
	trunkBefore := jjTest(t, f.repo, "log", "--no-graph", "-r", mainOrigin, "-T", "commit_id")
	localMainBefore := jjTest(t, f.repo, "log", "--no-graph", "-r", "main", "-T", "commit_id")
	var out bytes.Buffer
	err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: "feat(rjc): origin moved line", bodyFile: f.bodyFile, dryRun: true}, f, &out)
	if err != nil {
		t.Fatalf("dry run failed after origin moved: %v", err)
	}
	if got := ownedLocal(); got != ownedBefore {
		t.Fatalf("dry run moved the owned local bookmark: before=%s after=%s", ownedBefore, got)
	}
	if got := jjTest(t, f.repo, "log", "--no-graph", "-r", mainOrigin, "-T", "commit_id"); got == trunkBefore {
		t.Fatal("dry run did not fetch: main@origin stayed on the old commit, so the preflight judged a stale origin")
	}
	if got := jjTest(t, f.repo, "log", "--no-graph", "-r", "main", "-T", "commit_id"); got == localMainBefore {
		t.Fatal("dry run did not fast-forward the tracked local main that origin moved")
	}
	if len(f.pushes) != 1 {
		t.Fatalf("dry run pushed: %v", f.pushes)
	}
}

func TestSubmitReviewFixDryRunDoesNotPush(t *testing.T) {
	t.Parallel()
	f, parent, _ := publishedReviewFixStack(t)
	jjTest(t, f.repo, "new", quoteRev(parent))
	f.commit(t, "parent-review-fix.txt")
	before := opLogID(t, f)
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: parent, title: "feat(rjc): add review-fix parent", bodyFile: f.bodyFile, dryRun: true}, f, io.Discard); err != nil {
		t.Fatalf("review-fix dry run refused: %v", err)
	}
	if after := opLogID(t, f); after != before {
		t.Fatalf("review-fix dry run changed the op log: before=%s after=%s", before, after)
	}
	if len(f.pushes) != 2 {
		t.Fatalf("dry-run pushed: %v", f.pushes)
	}
}

// TestSubmitDryRunAdoptionReportsWouldCreateBookmark covers the unbookmarked
// adoption path. A real submit runs jj new and creates a bookmark. A dry run
// must report that and change nothing.
func TestSubmitDryRunAdoptionReportsWouldCreateBookmark(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	f.commit(t, "unbookmarked.txt")
	before := opLogID(t, f)
	bookmarksBefore := jjTest(t, f.repo, "bookmark", "list", "--all")
	pushes := len(f.pushes)
	var out bytes.Buffer
	err := f.repo.submit(f.ctx, submitOptions{issue: "EX-1", title: "feat(rjc): adopt unbookmarked work", bodyFile: f.bodyFile, dryRun: true}, f, &out)
	if err != nil {
		t.Fatalf("adoption dry run refused: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("would create bookmark")) {
		t.Fatalf("dry run did not report would create bookmark: %q", out.String())
	}
	if after := opLogID(t, f); after != before {
		t.Fatalf("adoption dry run changed the op log: before=%s after=%s", before, after)
	}
	if after := jjTest(t, f.repo, "bookmark", "list", "--all"); after != bookmarksBefore {
		t.Fatalf("adoption dry run changed bookmarks:\nbefore:\n%s\nafter:\n%s", bookmarksBefore, after)
	}
	if len(f.pushes) != pushes {
		t.Fatalf("adoption dry run pushed: %v", f.pushes)
	}
}
