package main

import (
	"io"
	"testing"
)

func TestNewOnOwnedPublishedAheadUsesLocalParent(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	parent := f.new(t, "EX-1", "feat(rjc): add ahead parent", "")
	f.commit(t, "parent.txt")
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", parent)
	originTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(parent)+"@origin", "-T", "commit_id")
	f.commit(t, "parent-next.txt")
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
	localTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(parent), "-T", "commit_id")
	if localTip == originTip {
		t.Fatal("setup did not advance the local parent past origin")
	}
	f.commit(t, "child.txt")
	work := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	child, err := f.repo.newChange(f.ctx, newOptions{issue: "EX-2", title: "feat(rjc): adopt ahead child", on: parent, at: work})
	if err != nil {
		t.Fatal(err)
	}
	base := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(child)+"-", "-T", "commit_id")
	if base != localTip {
		t.Fatalf("child base = %s, want local parent %s (origin %s)", base, localTip, originTip)
	}
	if saved := savedParent(t, f, child); saved.Bookmark != parent || saved.Tip != localTip {
		t.Fatalf("saved parent = %+v, want %s at %s", saved, parent, localTip)
	}
	if err := f.repo.saveCandidate(f.ctx, parent, "feat(rjc): add ahead parent", f.bodyFile, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: child, title: "feat(rjc): adopt ahead child", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	line, err := f.repo.line(f.ctx, child, lineOptions{})
	if err != nil || len(line) != 2 || line[0].Bookmark != parent || line[1].Bookmark != child {
		t.Fatalf("line after submit = %v, %v; want [%s %s]", line, err, parent, child)
	}
	if pushed := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(parent)+"@origin", "-T", "commit_id"); pushed != localTip {
		t.Fatalf("parent@origin = %s after submit, want local tip %s", pushed, localTip)
	}
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: child, title: "feat(rjc): adopt ahead child", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatalf("second submit: %v", err)
	}
}

func TestNewBaseOwnedParentPosition(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		publish bool
		// mutate adjusts the parent bookmark after the optional initial push.
		mutate func(t *testing.T, f *fixture, parent string)
		want   func(parent string) string
	}{
		{
			name:    "equal returns the local bookmark",
			publish: true,
			mutate: func(t *testing.T, f *fixture, parent string) {
				t.Helper()
			},
			want: quoteRev,
		},
		{
			name:    "ahead returns the local bookmark",
			publish: true,
			mutate: func(t *testing.T, f *fixture, parent string) {
				t.Helper()
				f.commit(t, "parent-next.txt")
				jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
			},
			want: quoteRev,
		},
		{
			name:    "behind returns origin",
			publish: true,
			mutate: func(t *testing.T, f *fixture, parent string) {
				t.Helper()
				f.commit(t, "parent-next.txt")
				jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
				jjTest(t, f.repo, "git", "push", "--bookmark", parent)
				jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@--", "--allow-backwards")
			},
			want: func(parent string) string { return quoteRev(parent) + "@origin" },
		},
		{
			name:    "deleted local bookmark returns origin",
			publish: true,
			mutate: func(t *testing.T, f *fixture, parent string) {
				t.Helper()
				jjTest(t, f.repo, "bookmark", "delete", parent)
			},
			want: func(parent string) string { return quoteRev(parent) + "@origin" },
		},
		{
			name: "unpublished parent returns the local bookmark",
			mutate: func(t *testing.T, f *fixture, parent string) {
				t.Helper()
			},
			want: quoteRev,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := parallelFixtureRepo(t)
			parent := f.new(t, "EX-1", "feat(rjc): base position parent", "")
			f.commit(t, "parent.txt")
			jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
			if tc.publish {
				jjTest(t, f.repo, "git", "push", "--bookmark", parent)
			}
			tc.mutate(t, f, parent)
			base, err := f.repo.newBase(f.ctx, parent)
			if err != nil {
				t.Fatal(err)
			}
			if want := tc.want(parent); base != want {
				t.Fatalf("newBase(%s) = %s, want %s", parent, base, want)
			}
		})
	}
}

func TestNewOnDivergedPublishedParentKeepsOrigin(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	parent := f.new(t, "EX-1", "feat(rjc): add diverged parent", "")
	f.commit(t, "parent.txt")
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", parent)
	jjTest(t, f.repo, "new", mainOrigin)
	f.commit(t, "rewritten.txt")
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-", "--allow-backwards")
	f.commit(t, "child.txt")
	work := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	base, err := f.repo.newBase(f.ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	if want := quoteRev(parent) + "@origin"; base != want {
		t.Fatalf("diverged base = %s, want %s", base, want)
	}
	_, err = f.repo.newChange(f.ctx, newOptions{issue: "EX-2", title: "feat(rjc): adopt diverged child", on: parent, at: work})
	assertRefusalGate(t, err, "existing-bookmark")
}
