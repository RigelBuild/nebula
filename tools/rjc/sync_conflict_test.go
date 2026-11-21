package main

import (
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSyncConflictPrintsRestore(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): conflicting child", "")
	writeTestFile(t, filepath.Join(f.repo.root, "README.md"), "child\n")
	jjTest(t, f.repo, "commit", "-m", "fix: child")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	oldTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	jjTest(t, f.repo, "new", mainOrigin)
	writeTestFile(t, filepath.Join(f.repo.root, "README.md"), "trunk\n")
	jjTest(t, f.repo, "commit", "-m", "fix: trunk")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	jjTest(t, f.repo, "new", quoteRev(bookmark))
	plan, err := f.repo.planSync(f.ctx, bookmark)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.repo.sync(f.ctx, syncOptions{bookmark: bookmark}, io.Discard)
	assertRefusalGate(t, err, "conflict")
	failure := err.(*failure)
	if !strings.Contains(failure.fix, "jj new ") || !strings.Contains(failure.fix, "jj bookmark set ") {
		t.Fatalf("missing fixes: %s", failure.fix)
	}
	if strings.Contains(failure.fix, "jj op ") {
		t.Fatalf("unsafe operation recovery: %s", failure.fix)
	}
	runSyncRecovery(t, f, failure.fix, plan)
	if got := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id"); got != oldTip {
		t.Fatalf("recovery tip = %s, want %s", got, oldTip)
	}
}

func TestRebaseConflictAbandonsEveryRewrittenSegmentCommit(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): two commit segment", "")
	writeTestFile(t, filepath.Join(f.repo.root, "README.md"), "lower\n")
	jjTest(t, f.repo, "commit", "-m", "fix: lower")
	writeTestFile(t, filepath.Join(f.repo.root, "README.md"), "tip\n")
	jjTest(t, f.repo, "commit", "-m", "fix: tip")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	segment, err := f.repo.revisions(f.ctx, quoteRev(mainOrigin)+".."+quoteRev(bookmark))
	if err != nil {
		t.Fatal(err)
	}
	if len(segment) < 2 {
		t.Fatalf("segment has %d commits, want at least two", len(segment))
	}
	jjTest(t, f.repo, "new", mainOrigin)
	writeTestFile(t, filepath.Join(f.repo.root, "README.md"), "trunk\n")
	jjTest(t, f.repo, "commit", "-m", "fix: trunk")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	line := []Change{{Bookmark: bookmark, Tip: tip, Base: mainOrigin, Owned: true}}
	err = f.repo.rebaseOwned(f.ctx, line, false, nil)
	assertRefusalGate(t, err, "conflict")
	detail, ok := errors.AsType[*failure](err)
	if !ok {
		t.Fatalf("missing conflict recovery: %v", err)
	}
	runSyncRecovery(t, f, detail.fix, syncPlan{Segments: []syncSegment{{Bookmark: bookmark, Commits: segment}}})
	changes := make([]string, 0, len(segment))
	for _, commit := range segment {
		changes = append(changes, "change_id("+quoteRev(jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(commit), "-T", "change_id"))+")")
	}
	visible := strings.TrimSpace(jjTest(t, f.repo, "log", "--no-graph", "-r", "("+strings.Join(changes, " | ")+") ~ "+syncSet(segment), "-T", `commit_id ++ "\n"`))
	if visible != "" {
		t.Fatalf("rewritten segment copies stay visible after recovery:\n%s", visible)
	}
}

func TestSyncLogFailurePrintsRestore(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): test failed logging", "")
	f.commit(t, "child.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	jjTest(t, f.repo, "new", mainOrigin)
	f.commit(t, "upstream.txt")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	jjTest(t, f.repo, "new", quoteRev(bookmark))
	_, err := f.repo.sync(f.ctx, syncOptions{bookmark: bookmark}, failureWriter{})
	if err == nil {
		t.Fatal("expected log failure")
	}
	var detail *failure
	if !errors.As(err, &detail) || detail.code != 3 || !strings.Contains(detail.fix, "jj bookmark set ") {
		t.Fatalf("missing log-failure restore: %v", err)
	}
}

func TestSyncValidationRefusalPrintsRestore(t *testing.T) {
	t.Parallel()
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "current-parent", true: "stale-parent"}[stale], func(t *testing.T) {
			t.Parallel()
			f := parallelFixtureRepo(t)
			parent := f.new(t, "EX-1", "feat(rjc): add parent", "")
			f.commit(t, "parent.txt")
			jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
			var clone string
			if stale {
				jjTest(t, f.repo, "git", "push", "--bookmark", parent)
				clone = filepath.Join(t.TempDir(), "clone")
				remote := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(f.repo.root)))), "remote.git")
				jjTest(t, f.repo, "git", "clone", remote, clone)
			}
			child := f.new(t, "EX-2", "feat(rjc): add child", parent)
			f.commit(t, "child.txt")
			jjTest(t, f.repo, "bookmark", "move", child, "--to", "@-")
			if stale {
				other := *f.repo
				other.root = clone
				jjTest(t, &other, "bookmark", "track", parent+"@origin")
				jjTest(t, &other, "new", quoteRev(parent))
				writeTestFile(t, filepath.Join(other.root, "rewritten.txt"), "rewritten parent\n")
				jjTest(t, &other, "squash")
				jjTest(t, &other, "new", quoteRev(parent))
				writeTestFile(t, filepath.Join(other.root, "extra-parent.txt"), "extra parent\n")
				jjTest(t, &other, "commit", "-m", "feat: additional parent")
				jjTest(t, &other, "bookmark", "move", parent, "--to", "@-")
				jjTest(t, &other, "git", "push", "--bookmark", parent)
				jjTest(t, f.repo, "git", "fetch", "--remote", "origin")
				jjTest(t, f.repo, "bookmark", "move", parent, "--to", quoteRev(parent)+"@origin", "--allow-backwards")
			}
			jjTest(t, f.repo, "new", mainOrigin)
			f.commit(t, "upstream.txt")
			jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
			jjTest(t, f.repo, "git", "push", "--bookmark", "main")
			jjTest(t, f.repo, "new", quoteRev(child))
			before := make(map[string]string)
			identities := make(map[string]string)
			parents := make(map[string]string)
			for _, bookmark := range []string{parent, child} {
				before[bookmark] = jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
				identities[bookmark] = jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "change_id")
				parents[bookmark] = jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", `parents.map(|p| p.commit_id()).join("\n")`)
			}
			divergent := jjTest(t, f.repo, "log", "--no-graph", "-r", "divergent()", "-T", `change_id ++ "\n"`)
			f.repo.landedParents = map[string]string{child: parent}
			plan, err := f.repo.planSync(f.ctx, child)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.repo.sync(f.ctx, syncOptions{bookmark: child}, io.Discard)
			detail, ok := errors.AsType[*failure](err)
			if !ok || detail.code != 1 || detail.gate != "parent-boundary" {
				t.Fatalf("sync error = %v, want post-rebase parent-boundary refusal with exit 1", err)
			}
			if strings.Contains(detail.fix, "jj undo") || strings.Contains(detail.fix, "jj op ") {
				t.Fatalf("unsafe operation recovery: %s", detail.fix)
			}
			for _, bookmark := range []string{parent, child} {
				if got := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id"); got == before[bookmark] {
					t.Fatalf("%s did not move during sync", bookmark)
				}
			}
			jjTest(t, f.repo, "new", quoteRev(child))
			runSyncRecoveryWithWorkingCopy(t, f, detail.fix, plan)
			for _, bookmark := range []string{parent, child} {
				if got := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id"); got != before[bookmark] {
					t.Errorf("%s restored tip = %s, want %s", bookmark, got, before[bookmark])
				}
				if got := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "change_id"); got != identities[bookmark] {
					t.Errorf("%s change id changed", bookmark)
				}
				if got := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", `parents.map(|p| p.commit_id()).join("\n")`); got != parents[bookmark] {
					t.Errorf("%s parents changed", bookmark)
				}
			}
			if got := jjTest(t, f.repo, "log", "--no-graph", "-r", "divergent()", "-T", `change_id ++ "\n"`); got != divergent {
				t.Errorf("divergent changes after restore = %q, before = %q", got, divergent)
			}
		})
	}
}

func runSyncRecoveryWithWorkingCopy(t *testing.T, f *fixture, fix string, plan syncPlan) {
	t.Helper()
	created := []string{}
	for _, segment := range plan.Segments {
		for _, old := range segment.Commits {
			id := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(old), "-T", "change_id")
			current := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(id), "-T", "commit_id")
			if current != old {
				created = append(created, current)
			}
		}
	}
	wc := jjTest(t, f.repo, "log", "--no-graph", "-r", "@", "-T", "change_id")
	parent := jjTest(t, f.repo, "log", "--no-graph", "-r", "heads(::@- ~ ("+syncSet(created)+"))", "-T", "commit_id")
	runSyncRecovery(t, f, fix, plan)
	if got := jjTest(t, f.repo, "log", "--no-graph", "-r", "@", "-T", "change_id"); got != wc {
		t.Fatalf("working-copy change = %s, want %s", got, wc)
	}
	current := jjTest(t, f.repo, "log", "--no-graph", "-r", "@", "-T", "commit_id")
	if slices.Contains(created, current) {
		t.Fatalf("working copy is an abandoned commit: %s", current)
	}
	if got := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id"); got != parent {
		t.Fatalf("working-copy parent = %s, want %s", got, parent)
	}
}

func runSyncRecovery(t *testing.T, f *fixture, fix string, plan syncPlan) {
	t.Helper()
	want := []string{}
	for _, segment := range plan.Segments {
		for _, old := range segment.Commits {
			id := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(old), "-T", "change_id")
			current := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(id), "-T", "commit_id")
			if current != old {
				want = append(want, current)
			}
		}
	}
	slices.Sort(want)
	bookmarks, abandons := 0, 0
	for line := range strings.SplitSeq(fix, "\n") {
		args := strings.Fields(line)
		if len(args) < 2 || args[0] != "jj" {
			continue
		}
		switch args[1] {
		case "bookmark":
			if abandons != 0 {
				t.Fatal("bookmark restore follows abandon")
			}
			bookmarks++
		case "abandon":
			abandons++
			if len(args) < 3 || args[2] == "--ignore-immutable" {
				t.Fatalf("invalid abandon command: %s", line)
			}
			got := slices.Clone(args[2:])
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Fatalf("abandoned commits = %v, want %v", got, want)
			}
		default:
			continue
		}
		jjTest(t, f.repo, args[1:]...)
	}
	if bookmarks == 0 || abandons != 1 {
		t.Fatalf("invalid recovery commands: %s", fix)
	}
}

type failureWriter struct{}

func (failureWriter) Write([]byte) (int, error) { return 0, errors.New("log write failed") }
