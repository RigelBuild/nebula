package prgates

import (
	"strings"
	"testing"
)

const prefix = "EX"

var specDirs = []string{"docs/specs/", "notes/specs/"}

func TestBranchName(t *testing.T) {
	for _, tc := range []struct {
		name, branch string
		bot, pass    bool
	}{
		{name: "owned", branch: "example-user/ex-8621-rjc", pass: true},
		{name: "reserved", branch: "mintaka/ex-1-test"},
		{name: "main", branch: "main"},
		{name: "bot", branch: "renovate/dependency", bot: true, pass: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := BranchName(BranchFacts{HeadRef: tc.branch, AuthorIsBot: tc.bot, IssuePrefix: prefix}); got.Pass != tc.pass {
				t.Fatalf("result = %+v", got)
			}
		})
	}
}

func TestTitleIssueRef(t *testing.T) {
	for _, tc := range []struct {
		name, title string
		bot, pass   bool
	}{
		{name: "reference", title: "fix(ci): correct title (EX-123)", pass: true},
		{name: "multiple references", title: "fix(ci): correct title (EX-123, EX-456)", pass: true},
		{name: "case insensitive", title: "fix(ci): correct title (ex-123)", pass: true},
		{name: "missing", title: "fix(ci): correct title"},
		{name: "other prefix", title: "fix(ci): correct title (ZZ-123)"},
		{name: "bot", title: "dependency update", bot: true, pass: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := TitleIssueRef(TitleFacts{Title: tc.title, AuthorIsBot: tc.bot, IssuePrefix: prefix}); got.Pass != tc.pass {
				t.Fatalf("result = %+v", got)
			}
		})
	}
}

func TestSpecImpact(t *testing.T) {
	for _, tc := range []struct {
		name   string
		facts  SpecImpactFacts
		pass   bool
		reason string
	}{
		{name: "spec", facts: SpecImpactFacts{ChangedFiles: []string{"docs/specs/tools/rjc.md"}}, pass: true, reason: "touches-spec"},
		{name: "second spec dir", facts: SpecImpactFacts{ChangedFiles: []string{"notes/specs/x.md"}}, pass: true, reason: "touches-spec"},
		{name: "declared", facts: SpecImpactFacts{Body: "Spec-impact: none"}, pass: true, reason: "declared"},
		{name: "empty declaration", facts: SpecImpactFacts{Body: "Spec-impact:  \n"}, reason: "missing"},
		{name: "mid sentence", facts: SpecImpactFacts{Body: "Note Spec-impact: none"}, reason: "missing"},
		{name: "new design declared", facts: SpecImpactFacts{ChangedFiles: []string{"docs/designs/x/y.md"}, Body: "Spec-impact: none"}, pass: true, reason: "declared"},
		{name: "new design undeclared", facts: SpecImpactFacts{ChangedFiles: []string{"docs/designs/x/y.md"}}, reason: "missing"},
		{name: "deleted design", facts: SpecImpactFacts{ChangedFiles: []string{"docs/designs/old.md"}, Body: "Spec-impact: none"}, pass: true, reason: "declared"},
		{name: "bot", facts: SpecImpactFacts{HeadBranch: "renovate/go"}, pass: true, reason: "exempt-branch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.facts.SpecDirs = specDirs
			got := SpecImpact(tc.facts)
			if got.Pass != tc.pass || got.Reason != tc.reason {
				t.Fatalf("result = %+v", got)
			}
		})
	}
}

func TestMetadataBounds(t *testing.T) {
	for _, tc := range []struct {
		name, title, body string
		pass              bool
	}{
		{name: "lower limits", title: strings.Repeat("\u754c", 20), body: strings.Repeat("word ", 40), pass: true},
		{name: "upper limits", title: strings.Repeat("\u754c", 90), body: strings.Repeat("word ", 300), pass: true},
		{name: "trimmed title", title: " \t" + strings.Repeat("a", 20) + " \t", body: strings.Repeat("word ", 40), pass: true},
		{name: "embedded carriage return", title: "fix(rjc): first\rsecond", body: strings.Repeat("word ", 40)},
		{name: "trailing body whitespace", title: strings.Repeat("a", 20), body: strings.Repeat("word ", 40) + " \t\r\n", pass: true},
		{name: "short title", title: strings.Repeat("a", 19), body: strings.Repeat("word ", 40)},
		{name: "long title", title: strings.Repeat("a", 91), body: strings.Repeat("word ", 40)},
		{name: "multiline", title: "fix(rjc): first line\nsecond", body: strings.Repeat("word ", 40)},
		{name: "short body", title: strings.Repeat("a", 20), body: strings.Repeat("word ", 39)},
		{name: "long body", title: strings.Repeat("a", 20), body: strings.Repeat("word ", 301)},
		{name: "comments and headings excluded", title: strings.Repeat("a", 20), body: "<!-- hidden words -->\n## Heading words\n" + strings.Repeat("word ", 39)},
		{name: "footers excluded", title: strings.Repeat("a", 20), body: strings.Repeat("word ", 39) + "\nSpec-impact: none\nRefs EX-1\nCo-authored-by: Example User <user@example.com>"},
		{name: "stack cannot fill minimum", title: strings.Repeat("a", 20), body: strings.Repeat("word ", 39) + "\n<!-- start jj-vine stack -->\nextra stack words\n<!-- end jj-vine stack -->"},
		{name: "stack cannot exceed maximum", title: strings.Repeat("a", 20), body: strings.Repeat("word ", 300) + "\n<!-- start jj-vine stack -->\nextra stack words\n<!-- end jj-vine stack -->", pass: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MetadataBounds(BoundsFacts{Title: tc.title, Body: tc.body, IssuePrefix: prefix}); got.Pass != tc.pass {
				t.Fatalf("result = %+v", got)
			}
		})
	}
}

func TestMissingIssuePrefixFails(t *testing.T) {
	for _, got := range []Result{
		BranchName(BranchFacts{HeadRef: "example-user/ex-1-test"}),
		TitleIssueRef(TitleFacts{Title: "fix(ci): correct title (EX-1)"}),
		MetadataBounds(BoundsFacts{Title: strings.Repeat("a", 20), Body: strings.Repeat("word ", 40), IssuePrefix: "E("}),
	} {
		if got.Pass || got.Reason != "issue-prefix" {
			t.Fatalf("result = %+v, want issue-prefix failure", got)
		}
	}
}

func TestSpecImpactRejectsBadSpecDirs(t *testing.T) {
	for _, tc := range []struct {
		name string
		dirs []string
	}{
		{name: "nil", dirs: nil},
		{name: "empty entry", dirs: []string{""}},
		{name: "no trailing slash", dirs: []string{"docs/specs"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SpecImpact(SpecImpactFacts{ChangedFiles: []string{"docs/specs/x.md"}, Body: "Spec-impact: none", SpecDirs: tc.dirs})
			if got.Pass || got.Reason != "spec-dirs" {
				t.Fatalf("result = %+v, want spec-dirs failure", got)
			}
		})
	}
}

func TestMessagesNameIssuePrefix(t *testing.T) {
	if got := TitleIssueRef(TitleFacts{Title: "fix(ci): no reference", IssuePrefix: "ex"}); !strings.Contains(got.Message, "'EX-NNN'") {
		t.Fatalf("title message = %q", got.Message)
	}
	if got := BranchName(BranchFacts{HeadRef: "main", IssuePrefix: prefix}); !strings.Contains(got.Message, "<codename>/ex-<n>-") {
		t.Fatalf("branch message = %q", got.Message)
	}
}
