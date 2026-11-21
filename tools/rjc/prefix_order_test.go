package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// repoState captures every jj-visible write rjc could make: commits, bookmarks, remote bookmarks and the operation head.
func repoState(t *testing.T, f *fixture) string {
	t.Helper()
	return opLogID(t, f) + "\n" + jjTest(t, f.repo, "log", "--no-graph", "-r", "all()", "-T", "commit_id ++ bookmarks ++ \"\\n\"")
}

// TestAuthoredIssueRefusedBeforeWrites proves new, meta and submit each run the authored-title issue check, which
// uses the profile's issuePrefix, before any write. composeTitle no longer checks for issue references itself.
func TestAuthoredIssueRefusedBeforeWrites(t *testing.T) {
	f := fixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): linked change", "")
	f.commit(t, "linked.txt")
	for _, args := range [][]string{
		{"new", "--issue", "EX-2", "--title", "feat(rjc): other change (EX-9)"},
		{"meta", bookmark, "--title", "feat(rjc): linked change (EX-9)", "--body-file", f.bodyFile},
		{"submit", bookmark, "--title", "feat(rjc): linked change EX-9", "--body-file", f.bodyFile},
	} {
		t.Run(args[0], func(t *testing.T) {
			before := repoState(t, f)
			var out bytes.Buffer
			if code := run(f.ctx, args, &out, &out); code != 1 || !strings.Contains(out.String(), "authored title contains an issue reference") {
				t.Fatalf("exit=%d output=%s", code, &out)
			}
			if after := repoState(t, f); after != before || len(f.pushes) != 0 {
				t.Fatalf("refusal wrote state or pushed %v:\n%s\n->\n%s", f.pushes, before, after)
			}
		})
	}
}

// TestBadP0ReasonRefusedBeforeWrites proves a p0: reason whose prefix is not the repository's issuePrefix, or whose
// shape is wrong, is refused before any rebase, bookmark change, push, Linear lookup or forge call.
func TestBadP0ReasonRefusedBeforeWrites(t *testing.T) {
	for _, skip := range []string{"p0:OTHER-1", "p0:EX-0", "p0:EX-1 x", "p0:"} {
		t.Run(skip, func(t *testing.T) {
			f := fixtureRepo(t)
			bookmark := f.new(t, "EX-1", "feat(rjc): urgent change", "")
			f.commit(t, "urgent.txt")
			// Put main ahead so a submit that got past validation would rebase the line.
			jjTest(t, f.repo, "new", "main")
			f.commit(t, "main-ahead.txt")
			jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
			jjTest(t, f.repo, "git", "push", "--bookmark", "main")
			jjTest(t, f.repo, "new", bookmark)
			lookups := 0
			f.repo.lookupLinearPriority = func(context.Context, string) (int, string, bool, error) {
				lookups++
				return 1, "started", true, nil
			}
			before := repoState(t, f)
			reads := f.repoReads
			err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: "feat(rjc): urgent change", bodyFile: f.bodyFile, skip: skip, skipGiven: true}, f, io.Discard)
			var fail *failure
			if !errors.As(err, &fail) || fail.gate != "skip-reason" {
				t.Fatalf("submit(%q) = %v, want a skip-reason refusal", skip, err)
			}
			if after := repoState(t, f); after != before {
				t.Fatalf("refusal changed repository state:\n%s\n->\n%s", before, after)
			}
			if len(f.pushes) != 0 || lookups != 0 || f.repoReads != reads || f.pullCreates != 0 {
				t.Fatalf("refusal reached a remote: pushes=%v lookups=%d repoReads=%d->%d creates=%d", f.pushes, lookups, reads, f.repoReads, f.pullCreates)
			}
		})
	}
}
