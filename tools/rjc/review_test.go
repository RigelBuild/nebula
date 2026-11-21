package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUntrackedForeignParentRemainsBoundary(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	parent := f.new(t, "EX-1", "feat(rjc): preserve the foreign parent", "")
	foreign := "other-lane/ex-1-preserve-the-foreign-parent"
	jjTest(t, f.repo, "bookmark", "rename", parent, foreign)
	jjTest(t, f.repo, "git", "push", "--bookmark", foreign)
	jjTest(t, f.repo, "bookmark", "untrack", foreign+"@origin")
	jjTest(t, f.repo, "bookmark", "forget", foreign)
	before := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(foreign+"@origin"), "-T", "commit_id")
	child := f.new(t, "EX-2", "feat(rjc): submit an independent child", foreign)
	line, err := f.repo.line(f.ctx, child, lineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if line[0].Base != foreign+"@origin" {
		t.Fatalf("foreign boundary = %q", line[0].Base)
	}
	if _, err := f.repo.rebaseLine(f.ctx, line, false); err != nil {
		t.Fatal(err)
	}
	if got := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(foreign+"@origin"), "-T", "commit_id"); got != before {
		t.Fatalf("foreign parent was rewritten: %s -> %s", before, got)
	}
	ancestors, err := f.repo.revisions(f.ctx, quoteRev(before)+"::"+quoteRev(child))
	if err != nil || len(ancestors) < 2 {
		t.Fatalf("rebased child lost its foreign parent: ancestors=%v err=%v", ancestors, err)
	}
}

func TestForeignMetaRefusesBeforePush(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): refuse unsafe metadata changes", "")
	f.commit(t, "metadata.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	foreign := strings.Replace(bookmark, "test-lane/", "other-lane/", 1)
	jjTest(t, f.repo, "bookmark", "create", foreign, "-r", bookmark)
	err := f.repo.saveCandidate(f.ctx, foreign, "feat(rjc): reject foreign metadata", f.bodyFile, nil)
	var detail *failure
	if !errors.As(err, &detail) || detail.stage != "metadata" || detail.gate != "ownership" {
		t.Fatalf("foreign metadata refusal = %v", err)
	}
	for _, suffix := range []string{".metadata.json", ".candidate.md"} {
		path, err := (fileBaseline{root: f.repo.stateDir, keys: f.repo.keys}).path(foreign, suffix)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("foreign %s state changed: %v", suffix, err)
		}
	}
	jjTest(t, f.repo, "bookmark", "forget", foreign)
	opt := submitOptions{bookmark: bookmark, title: "feat(rjc): refuse unsafe metadata changes", bodyFile: f.bodyFile}
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestExistingPRBaseMustMatchSelectedLine(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): validate the existing PR base", "")
	f.commit(t, "base.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	opt := submitOptions{bookmark: bookmark, title: "feat(rjc): validate the existing PR base", bodyFile: f.bodyFile}
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	pr := f.prs[bookmark]
	metadata := f.metadata[pr]
	metadata.BaseRef = "other-lane/ex-2-unrelated"
	f.metadata[pr] = metadata
	before := len(f.pushes)
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err == nil || !strings.Contains(err.Error(), "base") {
		t.Fatalf("base refusal = %v", err)
	}
	if len(f.pushes) != before {
		t.Fatal("wrong-base PR reached the engine")
	}
}

func TestCandidateDiffUsesMergeBase(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): inspect only candidate changes", "")
	jjTest(t, f.repo, "new", "main@origin")
	writeTestFile(t, filepath.Join(f.repo.root, "docs/specs/unrelated.md"), "unrelated main spec\n")
	jjTest(t, f.repo, "commit", "-m", "docs: update an unrelated specification")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	jjTest(t, f.repo, "new", bookmark)
	body := strings.ReplaceAll(f.bodyFile, "body.md", "without-spec-impact.md")
	writeTestFile(t, body, "## What does this do\n\nA candidate changes tracked files.\n\n## Why do we need it\n\nThe review must compare the owned branch with its base.\n\n## Summary\n"+strings.Repeat("candidate words ", 25)+"\n## Verification\nReal jj checks.\n## Risks\nNo live forge.\n")
	c, err := f.repo.prepareCandidate(f.ctx, bookmark, "feat(rjc): inspect only candidate changes", body, "")
	if err != nil {
		t.Fatal(err)
	}
	line, err := f.repo.line(f.ctx, bookmark, lineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.repo.checkCandidate(f.ctx, line[0], c, ""); err == nil || !strings.Contains(err.Error(), "spec-impact") {
		t.Fatalf("unrelated main spec bypassed the gate: %v", err)
	}
}

func TestBodyHeadingUsesExactLineAndSection(t *testing.T) {
	for _, body := range []string{
		"Earlier prose mentions ## Summary with words.\n\n## What does this do\n\nA behavior change.\n\n## Why do we need it\n\nA real problem.\n\n## Summary\n\n## Verification\nChecked.\n\n## Risks\nNone.",
		"### Summary has content\n\n## What does this do\n\nA behavior change.\n\n## Why do we need it\n\nA real problem.\n\n## Summary\n\n## Verification\nChecked.\n\n## Risks\nNone.",
	} {
		if _, err := ComposeBody(exampleKeys(t), fallbackTemplate, body, []int{1}, "Example Author <author@example.com>"); err == nil || !strings.Contains(err.Error(), `body section "## Summary" is empty`) {
			t.Fatalf("expected empty Summary section, got %v", err)
		}
	}
}

func TestComposeAuthoredBodySkipReason(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): compose skip reason", "")
	f.commit(t, "feature.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	template, err := f.repo.template(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	authoredBytes, err := os.ReadFile(f.bodyFile)
	if err != nil {
		t.Fatal(err)
	}
	authored := string(authoredBytes)
	line, err := f.repo.line(f.ctx, bookmark, lineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	title, err := f.repo.keys.FullTitle("feat(rjc): compose skip reason", []int{1})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, reason, want string
	}{
		{name: "backtick", reason: "uses `code`", want: "`` uses `code` ``"},
		{name: "longest backtick run", reason: "uses ``code`` and `text`", want: "``` uses ``code`` and `text` ```"},
		{name: "internal backticks", reason: "uses `code` here", want: "``uses `code` here``"},
		{name: "leading backtick", reason: "`leading", want: "`` `leading ``"},
		{name: "trailing backtick", reason: "trailing`", want: "`` trailing` ``"},
		{name: "leading and trailing backticks", reason: "`both`", want: "`` `both` ``"},
		{name: "HTML comment", reason: "<!-- -->", want: "`<!-- -->`"},
		{name: "Markdown link", reason: "[x](y)", want: "`[x](y)`"},
		{name: "backslash", reason: `\`, want: "`\\`"},
		{name: "ampersand", reason: "&", want: "`&`"},
		{name: "Markdown punctuation", reason: "*_~", want: "`*_~`"},
		{name: "closing keyword mention and reference", reason: "fixes #1 @reviewer", want: "`fixes #1 @reviewer`"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body, err := f.repo.composeAuthoredBody(template, authored, tc.reason, []int{1})
			if err != nil {
				t.Fatal(err)
			}
			wantLine := "Checks skipped: " + tc.want
			gotLine := ""
			for bodyLine := range strings.SplitSeq(body, "\n") {
				if strings.HasPrefix(bodyLine, "Checks skipped:") {
					gotLine = bodyLine
					break
				}
			}
			if strings.Count(body, "Checks skipped:") != 1 || gotLine != wantLine {
				t.Fatalf("rendered skip line = %q, want %q in body %q", gotLine, wantLine, body)
			}
			candidate := candidate{title: title, body: body}
			if err := f.repo.checkCandidateWithInputs(f.ctx, line[0], candidate, tc.reason, template, []int{1}); err != nil {
				t.Fatalf("candidate rejected its composed body: %v", err)
			}

			replaced, err := f.repo.composeAuthoredBody(template, body, "fixes #2 @reviewer", []int{1})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(replaced, "Checks skipped:") != 1 || !strings.Contains(replaced, "\nChecks skipped: `fixes #2 @reviewer`\n") {
				t.Fatalf("re-composed skip annotation = %q", replaced)
			}

			withoutSkip, err := f.repo.composeAuthoredBody(template, replaced, "", []int{1})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(withoutSkip, "Checks skipped:") != 0 {
				t.Fatalf("empty skip left an annotation: %q", withoutSkip)
			}
		})
	}
}

func TestRepeatedSkipAnnotationReplacesOldReason(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): replace generated skip annotations", "")
	f.commit(t, "feature.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	template, err := f.repo.template(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	reason := "fixes #1 @reviewer"
	c, err := f.repo.prepareCandidate(f.ctx, bookmark, "feat(rjc): replace generated skip annotations", f.bodyFile, reason)
	if err != nil {
		t.Fatal(err)
	}
	line, err := f.repo.line(f.ctx, bookmark, lineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.repo.checkCandidateWithInputs(f.ctx, line[0], c, reason, template, []int{1}); err != nil {
		t.Fatalf("candidate rejected its composed body: %v", err)
	}
	body, err := f.repo.composeCandidate(f.ctx, bookmark, c.body, "main-red:issue-2")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(body, "Checks skipped:") != 1 || strings.Contains(body, reason) {
		t.Fatalf("skip annotation accumulated: %s", body)
	}
	withoutSkip, err := f.repo.composeCandidate(f.ctx, bookmark, body, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(withoutSkip, "Checks skipped:") != 0 {
		t.Fatalf("empty skip left an annotation: %s", withoutSkip)
	}
}

func TestApprovalCoverageUsesGlobalIndex(t *testing.T) {
	events := []TimelineEvent{{Index: 20, Event: "reviewed", State: "approved", ReviewID: 1}, {Index: 10, Event: "committed"}}
	if got := ApprovalCoverage(events, true); got != Covered {
		t.Fatalf("global timeline order = %v", got)
	}
	if events[0].Index != 20 {
		t.Fatal("coverage reordered the caller's timeline")
	}
}

func TestEngineBoundaryExcludesForeignParentWithSharedEmail(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	parent := f.new(t, "EX-1", "feat(rjc): preserve a foreign parent", "")
	foreign := strings.Replace(parent, "test-lane/", "other-lane/", 1)
	jjTest(t, f.repo, "bookmark", "rename", parent, foreign)
	jjTest(t, f.repo, "git", "push", "--bookmark", foreign)
	foreignTip := jjTest(t, f.repo, "log", "--no-graph", "-r", foreign, "-T", "commit_id")
	f.prs[foreign] = 99
	f.metadata[99] = Metadata{Title: "foreign title", Body: "foreign body", HeadRef: foreign, HeadSHA: foreignTip, BaseRef: "main"}
	child := f.new(t, "EX-2", "feat(rjc): bound submission to the owned child", foreign)
	f.commit(t, "child.txt")
	jjTest(t, f.repo, "bookmark", "move", child, "--to", "@-")
	line, err := f.repo.line(f.ctx, child, lineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.repo.checkEngineSelection(f.ctx, line[0], true); err != nil {
		t.Fatal(err)
	}
	opt := submitOptions{bookmark: child, title: "feat(rjc): bound submission to the owned child", bodyFile: f.bodyFile}
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(f.pushes) != 1 || f.pushes[0] != child {
		t.Fatalf("foreign parent was included: pushes=%v", f.pushes)
	}
	if f.metadata[99].HeadSHA != foreignTip || f.metadata[99].Body != "foreign body" {
		t.Fatal("foreign PR was mutated")
	}
}
