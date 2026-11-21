package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSubmitRetargetMovesBookmarkAndPushesSelectedCommit(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): rebuild submitted change", "")
	f.commit(t, "old.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	opt := submitOptions{bookmark: bookmark, title: "feat(rjc): rebuild submitted change", bodyFile: f.bodyFile}
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	oldTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	oldBase := f.metadata[f.prs[bookmark]].BaseRef

	jjTest(t, f.repo, "new", mainOrigin)
	f.commit(t, "rebuilt.txt")
	newTip := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	var log strings.Builder
	opt.at = newTip
	if err := f.repo.submit(f.ctx, opt, f, &log); err != nil {
		t.Fatal(err)
	}
	got := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	if got != newTip || len(f.pushes) != 2 || f.pushes[1] != bookmark || f.metadata[f.prs[bookmark]].HeadSHA != newTip {
		t.Fatalf("tip=%s pushes=%v metadata=%+v", got, f.pushes, f.metadata[f.prs[bookmark]])
	}
	if gotBase := f.metadata[f.prs[bookmark]].BaseRef; gotBase != oldBase {
		t.Fatalf("PR base=%s, want %s", gotBase, oldBase)
	}
	if !strings.Contains(log.String(), oldTip+" -> "+newTip) {
		t.Fatalf("retarget log missing old/new tips: %s", log.String())
	}
}

func TestSubmitRetargetRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		gate string
	}{
		{name: "bookmark not local", gate: "retarget-not-owned"},
		{name: "bookmark foreign", gate: "retarget-not-owned"},
		{name: "no open PR", gate: "retarget-not-owned"},
		{name: "wrong base", gate: "retarget-wrong-base"},
		{name: "unrelated base", gate: "retarget-wrong-base"},
		{name: "foreign bookmark in range", gate: "existing-bookmark"},
		{name: "merge in range", gate: "retarget-wrong-base"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := parallelFixtureRepo(t)
			bookmark := f.new(t, "EX-1", "feat(rjc): test retarget refusal", "")
			f.commit(t, "work.txt")
			jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
			f.prs[bookmark] = 1
			oldTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
			f.metadata[1] = Metadata{Title: "feat(rjc): test retarget refusal (EX-1)", Body: "## What does this do\n\nExisting body.\n\n## Why do we need it\n\nExisting problem.\n\n## Summary\n\nExisting summary.\n", HeadRef: bookmark, HeadSHA: oldTip, BaseRef: "main"}
			jjTest(t, f.repo, "git", "push", "--bookmark", bookmark)
			newRevision := oldTip
			target := bookmark
			switch tc.name {
			case "bookmark not local":
				jjTest(t, f.repo, "bookmark", "forget", bookmark)
			case "bookmark foreign":
				target = "other-lane/ex-1-change"
				jjTest(t, f.repo, "bookmark", "create", target, "-r", quoteRev(bookmark))
			case "no open PR":
				f.prs = map[string]int{}
			case "wrong base":
				newRevision = mainOrigin
			case "unrelated base":
				jjTest(t, f.repo, "new", "root()")
				f.commit(t, "unrelated.txt")
				newRevision = jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
			case "foreign bookmark in range":
				jjTest(t, f.repo, "new", mainOrigin)
				f.commit(t, "selected.txt")
				newRevision = jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
				jjTest(t, f.repo, "bookmark", "create", "other-lane/ex-2-foreign", "-r", quoteRev(newRevision))
			case "merge in range":
				first := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
				jjTest(t, f.repo, "new", mainOrigin)
				f.commit(t, "side.txt")
				second := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
				jjTest(t, f.repo, "new", first, second)
				f.commit(t, "merge.txt")
				newRevision = jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
			}
			before := jjTest(t, f.repo, "log", "--no-graph", "-r", "all()", "-T", "commit_id ++ bookmarks")
			err := f.repo.submit(f.ctx, submitOptions{bookmark: target, at: newRevision, title: "feat(rjc): test retarget refusal", bodyFile: f.bodyFile}, f, io.Discard)
			assertRefusalGate(t, err, tc.gate)
			if len(f.pushes) != 0 {
				t.Fatalf("refusal pushed bookmarks: %v", f.pushes)
			}
			after := jjTest(t, f.repo, "log", "--no-graph", "-r", "all()", "-T", "commit_id ++ bookmarks")
			if before != after {
				t.Fatalf("refusal changed history: before=%s after=%s", before, after)
			}
		})
	}
}

func TestSubmitRetargetStackBottomThenTop(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	lower := f.new(t, "EX-1", "feat(rjc): rebuilt lower", "")
	f.commit(t, "lower-old.txt")
	jjTest(t, f.repo, "bookmark", "move", lower, "--to", "@-")
	upper, err := f.repo.newChange(f.ctx, newOptions{issue: "EX-2", title: "feat(rjc): rebuilt upper", on: lower})
	if err != nil {
		t.Fatal(err)
	}
	f.commit(t, "upper-old.txt")
	jjTest(t, f.repo, "bookmark", "move", upper, "--to", "@-")
	out := strings.Builder{}
	if err := f.repo.saveCandidate(f.ctx, lower, "feat(rjc): rebuilt lower", f.bodyFile, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: upper, title: "feat(rjc): rebuilt upper", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}

	jjTest(t, f.repo, "new", mainOrigin)
	f.commit(t, "lower-new.txt")
	newLower := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: lower, at: newLower, title: "feat(rjc): rebuilt lower", bodyFile: f.bodyFile}, f, &out); err != nil {
		t.Fatal(err)
	}
	jjTest(t, f.repo, "new", quoteRev(lower))
	f.commit(t, "upper-new.txt")
	newUpper := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	jjTest(t, f.repo, "bookmark", "create", "other-lane/ex-3-foreign", "-r", quoteRev(newUpper))
	before := opLogID(t, f)
	err = f.repo.submit(f.ctx, submitOptions{bookmark: upper, at: newUpper, title: "feat(rjc): rebuilt upper", bodyFile: f.bodyFile}, f, &out)
	assertRefusalGate(t, err, "existing-bookmark")
	if after := opLogID(t, f); before != after {
		t.Fatalf("upper retarget refusal changed op log: before=%s after=%s", before, after)
	}
	jjTest(t, f.repo, "bookmark", "forget", "other-lane/ex-3-foreign")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: upper, at: newUpper, title: "feat(rjc): rebuilt upper", bodyFile: f.bodyFile}, f, &out); err != nil {
		t.Fatal(err)
	}
	if got := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(lower), "-T", "commit_id"); got != newLower {
		t.Fatalf("lower tip=%s, want %s", got, newLower)
	}
	if got := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(upper), "-T", "commit_id"); got != newUpper {
		t.Fatalf("upper tip=%s, want %s", got, newUpper)
	}
	if len(f.pushes) < 4 || f.pushes[len(f.pushes)-2] != lower || f.pushes[len(f.pushes)-1] != upper {
		t.Fatalf("stack retarget pushes=%v", f.pushes)
	}
}

func TestSubmitRetargetStackWithFastLocalHooks(t *testing.T) {
	f := fixtureRepo(t)
	installFastTrunk(t, f)
	lower := f.new(t, "EX-1", "feat(rjc): rebuilt lower", "")
	f.commit(t, "lower-old.txt")
	jjTest(t, f.repo, "bookmark", "move", lower, "--to", "@-")
	upper, err := f.repo.newChange(f.ctx, newOptions{issue: "EX-2", title: "feat(rjc): rebuilt upper", on: lower})
	if err != nil {
		t.Fatal(err)
	}
	f.commit(t, "upper-old.txt")
	jjTest(t, f.repo, "bookmark", "move", upper, "--to", "@-")
	if err := f.repo.saveCandidate(f.ctx, lower, "feat(rjc): rebuilt lower", f.bodyFile, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: upper, title: "feat(rjc): rebuilt upper", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RJC_HOOK_LOCAL", "refuse")
	t.Setenv("RJC_HOOK_SSH", "")
	jjTest(t, f.repo, "new", mainOrigin)
	f.commit(t, "lower-new.txt")
	newLower := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: lower, at: newLower, title: "feat(rjc): rebuilt lower", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	jjTest(t, f.repo, "new", quoteRev(lower))
	f.commit(t, "upper-new.txt")
	newUpper := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: upper, at: newUpper, title: "feat(rjc): rebuilt upper", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(upper), "-T", "commit_id"); got != newUpper {
		t.Fatalf("upper tip=%s, want %s", got, newUpper)
	}
}

func TestSubmitRetargetRefusesUnsafeLocalHooksBeforeMutation(t *testing.T) {
	for _, tc := range []struct {
		name         string
		path         string
		intermediate bool
	}{
		{name: "non-fast head", path: "hk.pkl"},
		{name: "local override head", path: "hk.local.pkl"},
		{name: "unsafe intermediate bookmark", path: "hk.pkl", intermediate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fixtureRepo(t)
			installFastTrunk(t, f)
			bookmark := f.new(t, "EX-1", "feat(rjc): rebuild submitted change", "")
			f.commit(t, "old.txt")
			jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
			if err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: "feat(rjc): rebuild submitted change", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
				t.Fatal(err)
			}
			f.pushes = nil
			jjTest(t, f.repo, "new", mainOrigin)
			writeTestFile(t, filepath.Join(f.repo.root, tc.path), "unsafe hook\n")
			jjTest(t, f.repo, "commit", "-m", "fix: unsafe hook")
			if tc.intermediate {
				jjTest(t, f.repo, "bookmark", "create", f.repo.codename+"/ex-2-intermediate", "-r", "@-")
				data, err := os.ReadFile("../../hk.pkl")
				if err != nil {
					t.Fatal(err)
				}
				writeTestFile(t, filepath.Join(f.repo.root, "hk.pkl"), string(data))
				jjTest(t, f.repo, "commit", "-m", "fix: restore fast hook")
			}
			newTip := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
			oldTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
			t.Setenv("RJC_HOOK_LOCAL", "refuse")
			t.Setenv("RJC_HOOK_SSH", "")
			before := opLogID(t, f)
			err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, at: newTip, title: "feat(rjc): rebuild submitted change", bodyFile: f.bodyFile}, f, io.Discard)
			assertRefusalGate(t, err, "local-hooks-refused")
			if after := opLogID(t, f); before != after {
				t.Fatalf("operation changed: %s -> %s", before, after)
			}
			if got := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id"); got != oldTip {
				t.Fatalf("bookmark moved: %s -> %s", oldTip, got)
			}
			if len(f.pushes) != 0 {
				t.Fatalf("unexpected pushes: %v", f.pushes)
			}
		})
	}
}

func TestSubmitRetargetRefusesForeignIntermediateBookmarkBeforeMutation(t *testing.T) {
	f := fixtureRepo(t)
	installFastTrunk(t, f)
	bookmark := f.new(t, "EX-1", "feat(rjc): rebuild submitted change", "")
	f.commit(t, "old.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: "feat(rjc): rebuild submitted change", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	f.pushes = nil
	jjTest(t, f.repo, "new", mainOrigin)
	f.commit(t, "intermediate.txt")
	jjTest(t, f.repo, "bookmark", "create", "other/ex-2-intermediate", "-r", "@-")
	f.commit(t, "selected.txt")
	newTip := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	oldTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	before := opLogID(t, f)
	err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, at: newTip, title: "feat(rjc): rebuild submitted change", bodyFile: f.bodyFile}, f, io.Discard)
	assertRefusalGate(t, err, "existing-bookmark")
	if after := opLogID(t, f); before != after {
		t.Fatalf("operation changed: %s -> %s", before, after)
	}
	if got := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id"); got != oldTip {
		t.Fatalf("bookmark moved: %s -> %s", oldTip, got)
	}
	if len(f.pushes) != 0 {
		t.Fatalf("unexpected pushes: %v", f.pushes)
	}
}

func TestSubmitRetargetPreservesHistoryOnRefusal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		issue    string
		title    string
		retarget bool
		hookFail bool
	}{
		{name: "issue", issue: "EX-1", title: "feat(rjc): existing change"},
		{name: "long title", title: strings.Repeat("x", 300)},
		{name: "retarget flag", title: "feat(rjc): existing change", retarget: true},
		{name: "hook failure", title: "feat(rjc): existing change", hookFail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := parallelFixtureRepo(t)
			bookmark := f.new(t, "EX-1", "feat(rjc): existing change", "")
			f.commit(t, "old.txt")
			jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
			if err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: "feat(rjc): existing change", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
				t.Fatal(err)
			}
			jjTest(t, f.repo, "new", mainOrigin)
			f.commit(t, "new.txt")
			newTip := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
			before := jjTest(t, f.repo, "log", "--no-graph", "-r", "all()", "-T", "commit_id ++ bookmarks")
			pushesBefore := len(f.pushes)
			if tc.hookFail {
				f.repo.env = append(f.repo.env, "RJC_TEST_HOOK_FAIL=1")
			}
			err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, at: newTip, issue: tc.issue, title: tc.title, bodyFile: f.bodyFile, retarget: tc.retarget}, f, io.Discard)
			if err == nil {
				t.Fatal("expected refusal")
			}
			if tc.retarget {
				failure, ok := err.(*failure)
				if !ok || failure.code != 2 {
					t.Fatalf("got %v, want usage error", err)
				}
			}
			if tc.hookFail && !strings.Contains(err.Error(), "hooks-failed") {
				t.Fatalf("hook failure: %v", err)
			}
			if len(f.pushes) != pushesBefore {
				t.Fatalf("refusal reached engine: before=%v after=%v", pushesBefore, f.pushes)
			}
			after := jjTest(t, f.repo, "log", "--no-graph", "-r", "all()", "-T", "commit_id ++ bookmarks")
			if before != after {
				t.Fatalf("refusal changed history: before=%s after=%s", before, after)
			}
		})
	}
}

func TestRetargetRangeBookmarks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		bookmark    string
		parent      bool
		wantRefusal bool
	}{
		{name: "owned descendant", bookmark: "tester/ex-2-lower"},
		{name: "recorded parent", bookmark: "other/parent", parent: true},
		{name: "foreign bookmark", bookmark: "other/parent", wantRefusal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := parallelFixtureRepo(t)
			f.commit(t, "lower.txt")
			lower := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
			name := tc.bookmark
			if tc.name == "owned descendant" {
				name = f.repo.codename + "/ex-2-lower"
			}
			jjTest(t, f.repo, "bookmark", "create", name, "-r", lower)
			f.commit(t, "upper.txt")
			upper := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
			parent := ""
			if tc.parent {
				parent = name
			}
			err := f.repo.checkRetargetRange(f.ctx, f.repo.codename+"/ex-1-upper", parent, mainOrigin, upper)
			if tc.wantRefusal {
				assertRefusalGate(t, err, "existing-bookmark")
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRetargetWithoutOpenPRKeepsBaseGate(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): existing change", "")
	f.commit(t, "work.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, retarget: true, title: "feat(rjc): existing change", bodyFile: f.bodyFile}, f, io.Discard)
	assertRefusalGate(t, err, "base")
	detail, ok := err.(*failure)
	if !ok || detail.stage != "gate-state" {
		t.Fatalf("got %v, want gate-state refusal", err)
	}
}

// buildRestackedOwnedChain returns the published parent, child, and rebuilt tip.
func buildRestackedOwnedChain(t *testing.T, f *fixture) (parent, child, newParent string) {
	t.Helper()
	parent = f.new(t, "EX-1", "feat(rjc): parent line", "")
	f.commit(t, "parent-old.txt")
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: parent, title: "feat(rjc): parent line", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	oldParent := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(parent), "-T", "commit_id")
	child, err := f.repo.newChange(f.ctx, newOptions{issue: "EX-2", title: "feat(rjc): child line", on: parent})
	if err != nil {
		t.Fatal(err)
	}
	f.commit(t, "child-old.txt")
	jjTest(t, f.repo, "bookmark", "move", child, "--to", "@-")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: child, title: "feat(rjc): child line", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	jjTest(t, f.repo, "new", quoteRev(oldParent))
	f.commit(t, "parent-new.txt")
	newParent = jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	jjTest(t, f.repo, "rebase", "--ignore-immutable", "-s", quoteRev(child), "-d", newParent)
	return parent, child, newParent
}

// submitRestackedParent submits the rebuilt parent and verifies its new tip.
func submitRestackedParent(t *testing.T, f *fixture, parent, newParent string) {
	t.Helper()
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: parent, at: newParent, title: "feat(rjc): parent line", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatalf("submit parent at new tip: %v", err)
	}
	if got := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(parent), "-T", "commit_id"); got != newParent {
		t.Fatalf("parent tip=%s, want %s", got, newParent)
	}
}

func restackedOwnedChain(t *testing.T, f *fixture) (parent, child string) {
	t.Helper()
	parent, child, newParent := buildRestackedOwnedChain(t, f)
	submitRestackedParent(t, f, parent, newParent)
	return parent, child
}

func TestSubmitRetargetCarriedStaleWorkspace(t *testing.T) {
	t.Parallel()
	for _, edit := range []bool{false, true} {
		name := "workspace on old copy"
		if edit {
			name = "work above old copy"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := parallelFixtureRepo(t)
			parent, child, newParent := buildRestackedOwnedChain(t, f)
			path := filepath.Join(t.TempDir(), "stale")
			jjTest(t, f.repo, "workspace", "add", path, "--name", "stale", "-r", quoteRev(child+"@origin"))
			if edit {
				jjTest(t, &repository{root: path, env: f.repo.env}, "new", quoteRev(child+"@origin"))
				writeTestFile(t, filepath.Join(path, "above-stale.txt"), "changed\n")
				jjTest(t, &repository{root: path, env: f.repo.env}, "commit", "-m", "fix: workspace above stale copy")
			} else {
				jjTest(t, &repository{root: path, env: f.repo.env}, "edit", quoteRev(child+"@origin"))
			}
			before := opLogID(t, f)
			_, err := f.repo.prepareRetarget(f.ctx, parent, newParent)
			assertRefusalGate(t, err, "stale-copy-visible")
			if after := opLogID(t, f); before != after {
				t.Fatalf("refusal changed op log: before=%s after=%s", before, after)
			}
		})
	}
}

func TestSubmitAtAcceptsRestackedOwnedChild(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	parent, child := restackedOwnedChain(t, f)
	jjTest(t, f.repo, "new", quoteRev(child))
	childTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(child), "-T", "commit_id")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: child, title: "feat(rjc): child line", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatalf("submit restacked child: %v", err)
	}
	if got := f.metadata[f.prs[child]].HeadSHA; got != childTip {
		t.Fatalf("child PR head=%s, want %s", got, childTip)
	}
	if got := f.metadata[f.prs[child]].BaseRef; got != parent {
		t.Fatalf("child PR base=%s, want %s", got, parent)
	}
	if divergent := jjTest(t, f.repo, "log", "--no-graph", "-r", "divergent()", "-T", "commit_id"); divergent != "" {
		t.Fatalf("submit left divergent commits: %s", divergent)
	}
}

func TestSubmitPlainChildAfterMainAdvances(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	parent, child := restackedOwnedChain(t, f)
	jjTest(t, f.repo, "new", mainOrigin)
	f.commit(t, "main-advance.txt")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	jjTest(t, f.repo, "new", quoteRev(child))
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: child, title: "feat(rjc): child line", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatalf("submit child after main advanced: %v", err)
	}
	childTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(child), "-T", "commit_id")
	if got := f.metadata[f.prs[child]].HeadSHA; got != childTip {
		t.Fatalf("child PR head=%s, want %s", got, childTip)
	}
	if got := f.metadata[f.prs[child]].BaseRef; got != parent {
		t.Fatalf("child PR base=%s, want %s", got, parent)
	}
	if divergent := jjTest(t, f.repo, "log", "--no-graph", "-r", "divergent()", "-T", "commit_id"); divergent != "" {
		t.Fatalf("submit left divergent commits: %s", divergent)
	}
}

func TestSubmitRefusesPublishedCopyOutsideRebase(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"plain", "at", "above"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			at := mode == "at"
			f := parallelFixtureRepo(t)
			title := "feat(rjc): published copy outside rebase"
			bookmark := f.new(t, "EX-1", title, "")
			f.commit(t, "published.txt")
			jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
			if err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: title, bodyFile: f.bodyFile}, f, io.Discard); err != nil {
				t.Fatal(err)
			}
			oldMain := jjTest(t, f.repo, "log", "--no-graph", "-r", mainOrigin, "-T", "commit_id")
			// Clone C fetches the published change, rewrites it onto the older
			// main, and force-pushes it.
			other := cloneFixtureRepo(t, f.repo, "other")
			jjTest(t, other, "bookmark", "track", quoteRev(bookmark+"@origin"))
			jjTest(t, other, "describe", "-r", quoteRev(bookmark), "-m", "fix: rewritten elsewhere")
			jjTest(t, other, "rebase", "-r", quoteRev(bookmark), "-d", quoteRev(oldMain))
			jjTest(t, other, "git", "push", "--bookmark", bookmark)
			// The forge's PR head follows C's force-push.
			pr := f.prs[bookmark]
			meta := f.metadata[pr]
			meta.HeadSHA = jjTest(t, other, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
			f.metadata[pr] = meta
			// Clone A advances main, rewrites its local copy onto it, and fetches.
			jjTest(t, f.repo, "new", mainOrigin)
			f.commit(t, "main-advance.txt")
			jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
			jjTest(t, f.repo, "git", "push", "--bookmark", "main")
			jjTest(t, f.repo, "--ignore-immutable", "rebase", "-r", quoteRev(bookmark), "-d", mainOrigin)
			jjTest(t, f.repo, "--ignore-immutable", "describe", "-r", quoteRev(bookmark), "-m", "fix: rewritten locally")
			local := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
			jjTest(t, f.repo, "git", "fetch", "--remote", "origin")
			// The fetch conflicts the local bookmark; keep it on the local copy, as
			// a lane that resolves the conflict would.
			jjTest(t, f.repo, "bookmark", "set", bookmark, "-r", local, "--allow-backwards")
			if got := jjTest(t, f.repo, "log", "--no-graph", "-r", "divergent()", "-T", `commit_id ++ "\n"`); len(strings.Fields(got)) < 2 {
				t.Fatalf("fixture did not make the change divergent: %q", got)
			}
			published := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark+"@origin"), "-T", "commit_id")
			jjTest(t, f.repo, "new", quoteRev(local))
			if mode == "above" {
				// An unbookmarked commit above the local tip: plain submit would
				// move the bookmark onto it before any later check.
				f.commit(t, "above.txt")
			}
			opt := submitOptions{bookmark: bookmark, title: title, bodyFile: f.bodyFile}
			if at {
				opt.at = local
			}
			before := opLogID(t, f)
			pushesBefore := len(f.pushes)
			err := f.repo.submit(f.ctx, opt, f, io.Discard)
			assertRefusalGate(t, err, "divergent-line")
			detail, _ := errors.AsType[*failure](err)
			for _, want := range []string{local[:8], published[:8], "jj metaedit --update-change-id"} {
				if !strings.Contains(detail.reason+" "+detail.fix, want) {
					t.Fatalf("refusal does not name %s: %v; fix: %s", want, err, detail.fix)
				}
			}
			if after := opLogID(t, f); before != after {
				t.Fatalf("refusal changed op log: before=%s after=%s", before, after)
			}
			if got := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id"); got != local {
				t.Fatalf("refusal moved bookmark: %s -> %s", local, got)
			}
			if len(f.pushes) != pushesBefore {
				t.Fatalf("refusal reached engine: pushes=%v", f.pushes)
			}
		})
	}
}

func opLogID(t *testing.T, f *fixture) string {
	t.Helper()
	return jjTest(t, f.repo, "op", "log", "--no-graph", "--limit", "1", "-T", "id")
}

// ownedLine returns an owned bookmark on one committed change.
func ownedLine(t *testing.T, f *fixture) string {
	t.Helper()
	bookmark := f.new(t, "EX-1", "feat(rjc): protect owned line", "")
	f.commit(t, "owned.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: "feat(rjc): protect owned line", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	return bookmark
}

// ownedSiblingLine returns an owned line whose owned sibling branches off an
// interior commit. With publish, the sibling exists only on origin.
func ownedSiblingLine(t *testing.T, f *fixture, publish bool) string {
	t.Helper()
	bookmark := f.new(t, "EX-1", "feat(rjc): protect owned line", "")
	f.commit(t, "interior.txt")
	interior := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	f.commit(t, "tip.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: "feat(rjc): protect owned line", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	jjTest(t, f.repo, "new", quoteRev(interior), "-m", "feat: owned sibling")
	sibling := f.repo.codename + "/ex-2-sibling"
	jjTest(t, f.repo, "bookmark", "create", sibling, "-r", "@")
	if publish {
		jjTest(t, f.repo, "git", "push", "--bookmark", sibling)
		jjTest(t, f.repo, "bookmark", "forget", sibling)
	}
	return bookmark
}

func staleWorkspaceLine(t *testing.T, f *fixture) string {
	t.Helper()
	_, child := restackedOwnedChain(t, f)
	path := filepath.Join(t.TempDir(), "stale")
	jjTest(t, f.repo, "workspace", "add", path, "--name", "stale", "-r", quoteRev(child+"@origin"))
	other := &repository{root: path, env: f.repo.env}
	jjTest(t, other, "edit", quoteRev(child+"@origin"))
	return child
}

func TestSubmitRetargetStackedTargetPreflightRefusal(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	parent := f.new(t, "EX-1", "feat(rjc): parent line", "")
	f.commit(t, "parent-interior.txt")
	interior := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	f.commit(t, "parent-tip.txt")
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: parent, title: "feat(rjc): parent line", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	child, err := f.repo.newChange(f.ctx, newOptions{issue: "EX-2", title: "feat(rjc): child line", on: parent})
	if err != nil {
		t.Fatal(err)
	}
	f.commit(t, "child.txt")
	jjTest(t, f.repo, "bookmark", "move", child, "--to", "@-")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: child, title: "feat(rjc): child line", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	jjTest(t, f.repo, "new", quoteRev(interior))
	f.commit(t, "foreign.txt")
	jjTest(t, f.repo, "bookmark", "create", "other-lane/ex-3-foreign", "-r", "@-")
	jjTest(t, f.repo, "new", quoteRev(parent))
	f.commit(t, "rebuilt-child.txt")
	newTip := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	oldChildTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(child), "-T", "commit_id")
	before := opLogID(t, f)
	err = f.repo.submit(f.ctx, submitOptions{bookmark: child, at: newTip, title: "feat(rjc): child line", bodyFile: f.bodyFile}, f, io.Discard)
	assertRefusalGate(t, err, "foreign-descendant")
	if after := opLogID(t, f); before != after {
		t.Fatalf("refusal changed op log: before=%s after=%s", before, after)
	}
	if got := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(child), "-T", "commit_id"); got != oldChildTip {
		t.Fatalf("refusal moved target bookmark: got %s, want %s", got, oldChildTip)
	}
}

func TestRebaseRestackedLineConflictOmitsOperationRestoreHint(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	parent, child := restackedOwnedChain(t, f)
	parentTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(parent), "-T", "commit_id")
	childTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(child), "-T", "commit_id")
	jjTest(t, f.repo, "new", "main@origin")
	writeTestFile(t, filepath.Join(f.repo.root, "child-old.txt"), "conflicting main edit\n")
	jjTest(t, f.repo, "commit", "-m", "fix: conflict with restacked child")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	line := []Change{
		{Bookmark: parent, Tip: parentTip, Base: "main@origin", Owned: true},
		{Bookmark: child, Tip: childTip, Base: parent, Owned: true},
	}
	err := f.repo.rebaseOwned(f.ctx, line, false, nil)
	assertRefusalGate(t, err, "conflict")
	var detail *failure
	if !errors.As(err, &detail) || strings.Contains(detail.fix, "op restore") {
		t.Fatalf("conflict refusal must not suggest jj op restore: %+v", detail)
	}
	for _, want := range []string{
		"jj bookmark set " + parent + " -r " + parentTip + " --allow-backwards",
		"jj bookmark set " + child + " -r " + childTip + " --allow-backwards",
	} {
		if !strings.Contains(detail.fix, want) {
			t.Fatalf("conflict refusal must name recovery %q, got fix:\n%s", want, detail.fix)
		}
	}
}

func TestRetargetDescendantRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		gate  string
		build func(t *testing.T, f *fixture) string
	}{
		{name: "foreign local child", gate: "foreign-descendant", build: func(t *testing.T, f *fixture) string {
			t.Helper()
			bookmark := ownedLine(t, f)
			jjTest(t, f.repo, "bookmark", "create", "other-lane/ex-2-child", "-r", "@")
			return bookmark
		}},
		{name: "foreign origin-only child", gate: "foreign-published", build: func(t *testing.T, f *fixture) string {
			t.Helper()
			bookmark := ownedLine(t, f)
			jjTest(t, f.repo, "describe", "-m", "feat: foreign child")
			jjTest(t, f.repo, "bookmark", "create", "other-lane/ex-2-child", "-r", "@")
			jjTest(t, f.repo, "git", "push", "--bookmark", "other-lane/ex-2-child")
			jjTest(t, f.repo, "bookmark", "forget", "other-lane/ex-2-child")
			return bookmark
		}},
		{name: "owned origin-only child", gate: "foreign-published", build: func(t *testing.T, f *fixture) string {
			t.Helper()
			bookmark := ownedLine(t, f)
			jjTest(t, f.repo, "describe", "-m", "feat: owned child")
			child := f.repo.codename + "/ex-2-child"
			jjTest(t, f.repo, "bookmark", "create", child, "-r", "@")
			jjTest(t, f.repo, "git", "push", "--bookmark", child)
			jjTest(t, f.repo, "bookmark", "forget", child)
			return bookmark
		}},
		{name: "foreign work on old copy", gate: "foreign-descendant", build: func(t *testing.T, f *fixture) string {
			t.Helper()
			_, child := restackedOwnedChain(t, f)
			jjTest(t, f.repo, "new", quoteRev(child+"@origin"))
			f.commit(t, "foreign.txt")
			jjTest(t, f.repo, "bookmark", "create", "other-lane/ex-3-foreign", "-r", "@-")
			jjTest(t, f.repo, "new", "main@origin")
			return child
		}},
		{name: "unbookmarked work on old copy", gate: "foreign-descendant", build: func(t *testing.T, f *fixture) string {
			t.Helper()
			_, child := restackedOwnedChain(t, f)
			jjTest(t, f.repo, "new", quoteRev(child+"@origin"))
			f.commit(t, "work.txt")
			jjTest(t, f.repo, "new", "main@origin")
			return child
		}},
		{name: "stale copy workspace", gate: "stale-copy-visible", build: staleWorkspaceLine},
		{name: "unbookmarked described work", gate: "foreign-descendant", build: func(t *testing.T, f *fixture) string {
			t.Helper()
			bookmark := ownedLine(t, f)
			f.commit(t, "descendant.txt")
			return bookmark
		}},
		{name: "owned sibling", gate: "outside-line", build: func(t *testing.T, f *fixture) string {
			t.Helper()
			return ownedSiblingLine(t, f, false)
		}},
		{name: "owned origin-only sibling", gate: "foreign-published", build: func(t *testing.T, f *fixture) string {
			t.Helper()
			return ownedSiblingLine(t, f, true)
		}},
		{name: "sibling owned bookmarks above selected tip", gate: "outside-line", build: func(t *testing.T, f *fixture) string {
			t.Helper()
			bookmark := ownedLine(t, f)
			tip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
			for _, issue := range []string{"EX-2", "EX-3"} {
				jjTest(t, f.repo, "new", quoteRev(tip))
				f.commit(t, "sibling-"+strings.TrimPrefix(issue, "EX-")+".txt")
				name := f.repo.codename + "/ex-" + strings.TrimPrefix(issue, "EX-") + "-sibling"
				jjTest(t, f.repo, "bookmark", "create", name, "-r", "@-")
			}
			return bookmark
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := parallelFixtureRepo(t)
			bookmark := tc.build(t, f)
			before := opLogID(t, f)
			err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, at: bookmark, title: "feat(rjc): protect owned line", bodyFile: f.bodyFile}, f, io.Discard)
			assertRefusalGate(t, err, tc.gate)
			if after := opLogID(t, f); before != after {
				t.Fatalf("refusal changed op log: before=%s after=%s", before, after)
			}
		})
	}
}

func TestSubmitPlainDescendantRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		gate  string
		build func(t *testing.T, f *fixture) string
	}{
		{name: "foreign local child", gate: "foreign-descendant", build: func(t *testing.T, f *fixture) string {
			t.Helper()
			bookmark := ownedLine(t, f)
			jjTest(t, f.repo, "bookmark", "create", "other-lane/ex-2-child", "-r", "@")
			return bookmark
		}},
		{name: "foreign origin-only child", gate: "foreign-published", build: func(t *testing.T, f *fixture) string {
			t.Helper()
			bookmark := ownedLine(t, f)
			jjTest(t, f.repo, "describe", "-m", "feat: foreign child")
			jjTest(t, f.repo, "bookmark", "create", "other-lane/ex-2-child", "-r", "@")
			jjTest(t, f.repo, "git", "push", "--bookmark", "other-lane/ex-2-child")
			jjTest(t, f.repo, "bookmark", "forget", "other-lane/ex-2-child")
			return bookmark
		}},
		{name: "stale copy workspace", gate: "stale-copy-visible", build: staleWorkspaceLine},
		{name: "foreign child on old copy", gate: "foreign-descendant", build: func(t *testing.T, f *fixture) string {
			t.Helper()
			_, child := restackedOwnedChain(t, f)
			jjTest(t, f.repo, "new", quoteRev(child+"@origin"))
			f.commit(t, "foreign.txt")
			jjTest(t, f.repo, "bookmark", "create", "other-lane/ex-3-foreign", "-r", "@-")
			jjTest(t, f.repo, "new", "main@origin")
			return child
		}},
		{name: "unbookmarked work on old copy", gate: "foreign-descendant", build: func(t *testing.T, f *fixture) string {
			t.Helper()
			_, child := restackedOwnedChain(t, f)
			jjTest(t, f.repo, "new", quoteRev(child+"@origin"))
			f.commit(t, "work.txt")
			jjTest(t, f.repo, "new", "main@origin")
			return child
		}},
		{name: "owned sibling", gate: "foreign-descendant", build: func(t *testing.T, f *fixture) string {
			t.Helper()
			return ownedSiblingLine(t, f, false)
		}},
		{name: "owned origin-only sibling", gate: "foreign-published", build: func(t *testing.T, f *fixture) string {
			t.Helper()
			return ownedSiblingLine(t, f, true)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := parallelFixtureRepo(t)
			bookmark := tc.build(t, f)
			jjTest(t, f.repo, "new", quoteRev(bookmark))
			before := opLogID(t, f)
			err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: "feat(rjc): protect owned line", bodyFile: f.bodyFile}, f, io.Discard)
			assertRefusalGate(t, err, tc.gate)
			if after := opLogID(t, f); before != after {
				t.Fatalf("refusal changed op log: before=%s after=%s", before, after)
			}
		})
	}
}
