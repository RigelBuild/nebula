package main

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestProfileFor(t *testing.T) {
	const coAuthor = "Example Author <author@example.com>"
	for _, tc := range []struct {
		name, origin, repo, wantError string
		ciGates                       []string
	}{
		{name: "listed gates", origin: "https://github.com/example/app.git", repo: "example/app", ciGates: []string{"pr-title-issue-ref", "branch-name", "spec-impact", metadataBoundsGate}},
		{name: "case folded", origin: "https://github.com/Example/App.git", repo: "example/app", ciGates: []string{"pr-title-issue-ref", "branch-name", "spec-impact", metadataBoundsGate}},
		{name: "trailing slash", origin: "https://github.com/example/app/", repo: "example/app", ciGates: []string{"pr-title-issue-ref", "branch-name", "spec-impact", metadataBoundsGate}},
		{name: "ssh origin", origin: "git@github.com:example/docs.git", repo: "example/docs", ciGates: []string{metadataBoundsGate}},
		{name: "empty name", origin: "https://github.com/example/", wantError: "unsupported repository"},
		{name: "nested name", origin: "https://github.com/example/app/nested", wantError: "unsupported repository name"},
		{name: "dot name", origin: "https://github.com/example/..", wantError: "unsupported repository name"},
		{name: "invalid name", origin: "https://github.com/example/in%20valid", wantError: "unsupported repository name"},
		{name: "unlisted repo", origin: "https://github.com/example/unlisted.git", wantError: "is not listed in the profile file"},
		{name: "foreign owner", origin: "https://github.com/foreign/example.git", wantError: "unsupported repository owner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile, err := ProfileFor(loadExampleConfig(t), tc.origin)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error = %v, want %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if profile.Name != tc.repo || profile.CoAuthor != coAuthor ||
				!slices.Equal(profile.CIGates, tc.ciGates) ||
				!slices.Equal(profile.LocalGates, []string{"bookmark-name", "issue-id"}) {
				t.Fatalf("profile = %+v", profile)
			}
		})
	}
}

func TestProfileForDefaultAndOwnerCoAuthor(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test source directory")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(source), "testdata", "profile.cue"))
	if err != nil {
		t.Fatal(err)
	}
	profileCUE := string(data) + `

default: {
	issuePrefix: "EX"
	specDirs: ["docs/"]
}
coAuthorByOwner: {
	EXAMPLE: "Org Author <org@example.com>"
}
`
	path := filepath.Join(t.TempDir(), "profile.cue")
	writeTestFile(t, path, profileCUE)
	withDefaults, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		origin     string
		noDefault  bool
		oldFile    bool
		wantName   string
		wantPrefix string
		wantAuthor string
		wantError  string
	}{
		{
			name:       "unlisted repo gets default",
			origin:     "https://github.com/example/unlisted.git",
			wantName:   "example/unlisted",
			wantPrefix: "EX",
			wantAuthor: "Org Author <org@example.com>",
		},
		{
			name:      "unlisted repo with no default is refused",
			origin:    "https://github.com/example/unlisted.git",
			noDefault: true,
			wantError: "is not listed in the profile file",
		},
		{
			name:       "owner override wins over global coauthor",
			origin:     "https://github.com/example/app.git",
			wantName:   "example/app",
			wantPrefix: "EX",
			wantAuthor: "Org Author <org@example.com>",
		},
		{
			name:       "old profile file still loads",
			origin:     "https://github.com/example/app.git",
			oldFile:    true,
			wantName:   "example/app",
			wantPrefix: "EX",
			wantAuthor: "Example Author <author@example.com>",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := withDefaults
			if tt.noDefault {
				cfg.Default = nil
			}
			if tt.oldFile {
				cfg = loadExampleConfig(t)
			}
			profile, err := ProfileFor(cfg, tt.origin)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("ProfileFor error = %v, want %q", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if profile.Name != tt.wantName || profile.IssuePrefix != tt.wantPrefix || profile.CoAuthor != tt.wantAuthor {
				t.Fatalf("profile = %+v, want name %q, issue prefix %q, coauthor %q", profile, tt.wantName, tt.wantPrefix, tt.wantAuthor)
			}
		})
	}
}

func TestJJVineProfileSkipsHooks(t *testing.T) {
	cfg := loadExampleConfig(t)
	profile, err := ProfileFor(cfg, "https://github.com/example/jj-vine.git")
	if err != nil {
		t.Fatal(err)
	}
	want := "jj-vine has no pre-push hook config (EX-1)"
	if profile.Hooks || profile.NoHooksReason != want || profile.TrunkRemote() != "main@origin" {
		t.Fatalf("profile = %+v", profile)
	}
}

func TestNoHooksProfilesSkipWithValidReason(t *testing.T) {
	cfg := loadExampleConfig(t)
	var names []string
	for name, entry := range cfg.Repos {
		if !entry.Hooks {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	if len(names) == 0 {
		t.Fatal("testdata/profile.cue has no profile with hooks: false")
	}
	k := exampleKeys(t)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			profile, err := ProfileFor(cfg, "https://github.com/"+name+".git")
			if err != nil {
				t.Fatal(err)
			}
			if profile.Name != name || profile.Hooks || profile.NoHooksReason != cfg.Repos[name].NoHooksReason {
				t.Fatalf("profile = %+v, want hooks off with the profile reason", profile)
			}
			if !validSkipReason(k, strings.TrimSpace(profile.NoHooksReason)) {
				t.Fatalf("noHooksReason %q fails the --skip-checks validator", profile.NoHooksReason)
			}
		})
	}
}

func TestNaming(t *testing.T) {
	k := exampleKeys(t)
	title, err := k.FullTitle("feat(rjc): add safe submission", []int{8621})
	if err != nil || title != "feat(rjc): add safe submission (EX-8621)" {
		t.Fatalf("title = %q, %v", title, err)
	}
	bookmark, err := k.BookmarkName("homelab-move", 8621, title)
	if err != nil || bookmark != "homelab-move/ex-8621-add-safe-submission" {
		t.Fatalf("bookmark = %q, %v", bookmark, err)
	}
	if _, err := k.BookmarkName("renovate", 1, title); err == nil {
		t.Fatal("reserved codename accepted")
	}
	if _, err := k.FullTitle("feat: "+strings.Repeat("long ", 30), []int{1}); err == nil {
		t.Fatal("long title accepted")
	}
	if k.Owned("home", "home-other/ex-1-safe") || !k.Owned("home", "home/ex-1-safe") {
		t.Fatal("ownership is not prefix bounded")
	}
}

func TestConventionalTypesMatchCommitlint(t *testing.T) {
	wantTypes := []string{"build", "chore", "ci", "docs", "feat", "fix", "perf", "refactor", "revert", "style", "test"}
	k := exampleKeys(t)
	if !slices.Equal(conventionalTypes, wantTypes) {
		t.Fatalf("conventionalTypes = %v, want %v", conventionalTypes, wantTypes)
	}
	for _, typ := range wantTypes {
		t.Run(typ, func(t *testing.T) {
			title := typ + ": support existing titles"
			if _, err := k.FullTitle(title, []int{1}); err != nil {
				t.Fatal(err)
			}
			if _, err := k.BookmarkName("test-lane", 1, title); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMultipleIssues(t *testing.T) {
	k := exampleKeys(t)
	issues := []int{8635, 8636}
	title, err := k.FullTitle("docs(design): explain changes", issues)
	if err != nil || title != "docs(design): explain changes (EX-8635, EX-8636)" {
		t.Fatalf("title = %q, %v", title, err)
	}
	again, err := k.FullTitle(title, issues)
	if err != nil || again != title {
		t.Fatalf("reapplied title = %q, %v", again, err)
	}
	template := "## What does this do\n\n## Why do we need it\n\n## Summary\n"
	authored := "## What does this do\n\nChange summary.\n\n## Why do we need it\n\nThe fix resolves the problem.\n\n## Summary\n\nDescription\n\nRefs EX-8635\nRefs EX-8636\n"
	body, err := ComposeBody(k, template, authored, issues, "Example Author <author@example.com>")
	want := "## What does this do\n\nChange summary.\n\n## Why do we need it\n\nThe fix resolves the problem.\n\n## Summary\n\nDescription\n\nRefs EX-8635\nRefs EX-8636\n\nCo-authored-by: Example Author <author@example.com>\n"
	if err != nil || body != want {
		t.Fatalf("body = %q, %v", body, err)
	}
}

func TestMultipleIssuesFullSuffixExceedsTitleLimit(t *testing.T) {
	k := exampleKeys(t)
	title := "feat: " + strings.Repeat("é", 74)
	if _, err := k.FullTitle(title, []int{1}); err != nil {
		t.Fatalf("single issue title rejected: %v", err)
	}
	if _, err := k.FullTitle(title, []int{1, 2}); err == nil {
		t.Fatal("title exceeding the limit with both issues accepted")
	}
}

func TestAuthoredTitleRejectsIssue(t *testing.T) {
	k := exampleKeys(t)
	for _, title := range []string{"docs: changes (EX-8635)", "docs: changes (ex-1)", "docs: see EX-2 notes"} {
		if err := k.validateAuthoredTitle(title); err == nil {
			t.Errorf("%q: authored issue reference accepted", title)
		}
	}
	for _, title := range []string{"feat(ui): etex-12 layout", "feat: biex-3 cache"} {
		if err := k.validateAuthoredTitle(title); err != nil {
			t.Errorf("%q: refused a word containing ex-<n>: %v", title, err)
		}
	}
}

func TestApprovalCoverage(t *testing.T) {
	approve := TimelineEvent{Event: "reviewed", State: "approved", ReviewID: 12}
	push := TimelineEvent{Event: "committed"}
	for _, tc := range []struct {
		name     string
		events   []TimelineEvent
		complete bool
		want     Coverage
	}{
		{name: "commit after approval", events: []TimelineEvent{approve, push}, complete: true, want: Uncovered},
		{name: "force push after approval", events: []TimelineEvent{approve, {Event: "head_ref_force_pushed"}}, complete: true, want: Uncovered},
		{name: "approval after push", events: []TimelineEvent{push, approve}, complete: true, want: Covered},
		{name: "dismissed approval", events: []TimelineEvent{approve, {Event: "review_dismissed", DismissedReviewID: 12}}, complete: true, want: Uncovered},
		{name: "unmatched dismissal", events: []TimelineEvent{{Event: "review_dismissed", DismissedReviewID: 99}}, complete: true, want: Unknown},
		{name: "comment after push", events: []TimelineEvent{push, {Event: "reviewed", State: "commented"}}, complete: true, want: Uncovered},
		{name: "changes requested after push", events: []TimelineEvent{push, {Event: "reviewed", State: "changes_requested"}}, complete: true, want: Uncovered},
		{name: "incomplete pages", events: []TimelineEvent{approve}, want: Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ApprovalCoverage(tc.events, tc.complete); got != tc.want {
				t.Fatalf("coverage = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestComposeBody(t *testing.T) {
	k := exampleKeys(t)
	const what = "## What does this do"
	const why = "## Why do we need it"
	authored := what + "\n\nIt adds required PR body sections.\n\n" + why + "\n\nIt makes the change and its reason clear.\n\n## Summary\n\n" + strings.Repeat("word ", 40) + "\n\n## Spec impact\n\nSpec-impact: none\n"
	template := what + "\n\n" + why + "\n\n## Summary\n\n<!-- instructions -->\n\n## Spec impact\n"
	got, err := ComposeBody(k, template, authored, []int{8621}, "Example Author <author@example.com>")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "Refs EX-8621\n\nCo-authored-by: Example Author <author@example.com>\n") {
		t.Fatalf("body = %s", got)
	}
	for _, tc := range []struct {
		name, body, wantError string
	}{
		{name: "missing What section", body: strings.Replace(authored, what+"\n\nIt adds required PR body sections.\n\n", "", 1), wantError: `body file must fill template section "## What does this do" in order`},
		{name: "missing Why section", body: strings.Replace(authored, why+"\n\nIt makes the change and its reason clear.\n\n", "", 1), wantError: `body file must fill template section "## Why do we need it" in order`},
		{name: "empty What section", body: strings.Replace(authored, "It adds required PR body sections.\n\n", "", 1), wantError: `body section "## What does this do" is empty`},
		{name: "empty Why section", body: strings.Replace(authored, "It makes the change and its reason clear.\n\n", "", 1), wantError: `body section "## Why do we need it" is empty`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ComposeBody(k, template, tc.body, []int{8621}, "Example Author <author@example.com>"); err == nil || err.Error() != tc.wantError {
				t.Fatalf("ComposeBody() error = %v, want %q", err, tc.wantError)
			}
		})
	}
}
