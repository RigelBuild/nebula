package main

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestMovedForeignParentRefusesBeforeFirstSubmit(t *testing.T) {
	f := fixtureRepo(t)
	parent := f.new(t, "EX-1", "feat(rjc): preserve the selected parent", "")
	foreign := strings.Replace(parent, "test-lane/", "other-lane/", 1)
	jjTest(t, f.repo, "bookmark", "rename", parent, foreign)
	jjTest(t, f.repo, "git", "push", "--bookmark", foreign)
	child := f.new(t, "EX-2", "feat(rjc): retain the chosen parent relationship", foreign)
	f.commit(t, "child.txt")
	jjTest(t, f.repo, "bookmark", "move", child, "--to", "@-")
	before := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(child), "-T", "commit_id")
	originalIDs := persistedParentChangeIDs(t, f, child)
	jjTest(t, f.repo, "new", mainOrigin, "-m", "feat(other): replace the parent (EX-1)")
	jjTest(t, f.repo, "bookmark", "move", foreign, "--to", "@", "--allow-backwards")
	jjTest(t, f.repo, "git", "push", "--bookmark", foreign)
	jjTest(t, f.repo, "bookmark", "untrack", foreign+"@origin")
	jjTest(t, f.repo, "bookmark", "forget", foreign)
	jjTest(t, f.repo, "new", quoteRev(child))
	err := f.repo.submit(f.ctx, submitOptions{bookmark: child, title: "feat(rjc): retain the chosen parent relationship", bodyFile: f.bodyFile}, f, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "parent-boundary") || !strings.Contains(err.Error(), foreign) {
		t.Fatalf("moved-parent refusal = %v", err)
	}
	if len(f.pushes) != 0 || len(f.prs) != 0 {
		t.Fatal("moved parent reached push or forge writes")
	}
	if after := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(child), "-T", "commit_id"); after != before {
		t.Fatalf("child was rewritten before refusal: %s -> %s", before, after)
	}
	jjTest(t, f.repo, "rebase", "--ignore-immutable", "-s", before, "-d", quoteRev(foreign+"@origin"))
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: child, title: "feat(rjc): retain the chosen parent relationship", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatalf("restacked child could not submit: %v", err)
	}
	if len(f.pushes) != 1 || f.pushes[0] != child {
		t.Fatalf("restacked push set = %v", f.pushes)
	}
	selected := savedParent(t, f, child)
	remote := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(foreign+"@origin"), "-T", "commit_id")
	if selected.Bookmark != foreign || selected.Tip != remote {
		t.Fatalf("saved parent did not refresh: %+v, remote=%s", selected, remote)
	}
	known := persistedParentChangeIDs(t, f, child)
	currentIDs := strings.Fields(jjTest(t, f.repo, "log", "--no-graph", "-r", "main@origin.."+quoteRev(remote), "-T", `change_id ++ "\n"`))
	for _, id := range append(originalIDs, currentIDs...) {
		if !slices.Contains(known, id) {
			t.Fatalf("refreshed parent state omitted change ID %s: %v", id, known)
		}
	}
	if got := f.metadata[f.prs[child]].BaseRef; got != foreign {
		t.Fatalf("restacked PR base = %q, want %q", got, foreign)
	}
}

func TestPreflightDoesNotRecoverForbiddenStaleWorkspace(t *testing.T) {
	f := fixtureRepo(t)
	home := os.Getenv("HOME")
	outside := filepath.Join(home, "repos", "app")
	if err := os.MkdirAll(filepath.Dir(outside), 0o700); err != nil {
		t.Fatal(err)
	}
	jjTest(t, f.repo, "workspace", "add", outside, "--name", "forbidden", "-r", mainOrigin)
	other := &repository{root: outside}
	jjTest(t, other, "describe", "-m", "chore: forbidden workspace state")
	statePath := filepath.Join(outside, ".jj", "working_copy", "checkout")
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	jjTest(t, f.repo, "describe", "--ignore-working-copy", "-r", "forbidden@", "-m", "chore: stale forbidden workspace")
	t.Chdir(outside)
	if _, err := preflight(f.ctx, loadExampleConfig(t), "test-lane", io.Discard); err == nil || !strings.Contains(err.Error(), "write-boundary") {
		t.Fatalf("outside refusal = %v", err)
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("preflight recovered or changed a forbidden workspace")
	}
}

func TestPreflightStaleRepositoryValidation(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unsupported origin stays stale", true: "allowed origin recovers"}[allowed], func(t *testing.T) {
			f := fixtureRepo(t)
			root := f.repo.root + "-stale"
			jjTest(t, f.repo, "workspace", "add", root, "--name", "stale", "-r", mainOrigin)
			statePath := filepath.Join(root, ".jj", "working_copy", "checkout")
			before, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			jjTest(t, f.repo, "describe", "--ignore-working-copy", "-r", "stale@", "-m", "chore: make the other workspace stale")
			owner := "foreign"
			if allowed {
				owner = "example"
			}
			jjTest(t, f.repo, "--ignore-working-copy", "git", "remote", "set-url", "origin", "https://github.com/"+owner+"/app.git")
			t.Chdir(root)
			_, err = preflight(f.ctx, loadExampleConfig(t), "test-lane", io.Discard)
			if allowed && err != nil {
				t.Fatalf("allowed stale recovery failed: %v", err)
			}
			if !allowed {
				assertRefusalGate(t, err, "repository")
			}
			after, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			if (string(after) != string(before)) != allowed {
				t.Fatalf("checkout changed=%v, allowed=%v", string(after) != string(before), allowed)
			}
		})
	}
}
