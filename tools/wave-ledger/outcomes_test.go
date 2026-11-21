package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeGH struct {
	responses map[string]string
	errors    map[string]error
	calls     [][]string
}

func (f *fakeGH) Run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	key := strings.Join(args, " ")
	if err := f.errors[key]; err != nil {
		return nil, err
	}
	response, ok := f.responses[key]
	if !ok {
		return nil, errors.New("unexpected gh command")
	}
	return []byte(response), nil
}

func TestGHSourceUsesExactBookmarkMatchesAcrossRepos(t *testing.T) {
	runner := &fakeGH{responses: map[string]string{
		"pr list -R owner/one --head feature --state all --json url,state,headRefName": `[{"url":"https://github.com/owner/one/pull/1","state":"OPEN","headRefName":"feature-extra"},{"url":"https://github.com/owner/one/pull/2","state":"CLOSED","headRefName":"feature"}]`,
		"pr list -R owner/two --head feature --state all --json url,state,headRefName": `[{"url":"https://github.com/owner/two/pull/3","state":"MERGED","headRefName":"feature"}]`,
	}}
	source := NewGHSource(runner, []string{"owner/one", "owner/two"}, "EX", nil)
	prs, err := source.PRsForBookmark(t.Context(), "feature")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"owner/one#2", "owner/two#3"}; !reflect.DeepEqual(prs, want) {
		t.Fatalf("PRsForBookmark() = %v, want %v", prs, want)
	}
	wantCalls := [][]string{
		{"pr", "list", "-R", "owner/one", "--head", "feature", "--state", "all", "--json", "url,state,headRefName"},
		{"pr", "list", "-R", "owner/two", "--head", "feature", "--state", "all", "--json", "url,state,headRefName"},
	}
	if !reflect.DeepEqual(runner.calls, wantCalls) {
		t.Fatalf("gh args = %#v, want %#v", runner.calls, wantCalls)
	}
}

func TestGHSourceIssuePRsFiltersBookmarksAndTrailers(t *testing.T) {
	runner := &fakeGH{responses: map[string]string{
		"api --paginate --slurp repos/owner/repo/pulls?state=all&per_page=100": `[[{"number":10,"title":"Ex-42 note","body":"Refs EX-42","head":{"ref":"agents/ex-42-work"}},{"number":11,"title":"Unrelated","body":"Refs EX-420","head":{"ref":"agents/ex-420-work"}},{"number":13,"title":"Other prefix","body":"Refs OTHER-42","head":{"ref":"agents/other-42-work"}}],[{"number":12,"title":"No marker","body":"Refs EX-42","head":{"ref":"agents/other"}}]]`,
	}}
	source := NewGHSource(runner, []string{"owner/repo"}, "EX", nil)
	prs, err := source.IssuePRs(t.Context(), "EX-42")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"owner/repo#10", "owner/repo#12"}; !reflect.DeepEqual(prs, want) {
		t.Fatalf("IssuePRs() = %v, want %v", prs, want)
	}
	if want := [][]string{{"api", "--paginate", "--slurp", "repos/owner/repo/pulls?state=all&per_page=100"}}; !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("gh args = %#v, want %#v", runner.calls, want)
	}
}

func TestGHSourceIssuePRsRejectsOtherPrefix(t *testing.T) {
	runner := &fakeGH{responses: map[string]string{}}
	source := NewGHSource(runner, []string{"owner/repo"}, "EX", nil)
	if _, err := source.IssuePRs(t.Context(), "OTHER-42"); !errors.Is(err, errInvalidIssue) {
		t.Fatalf("IssuePRs() error = %v, want invalid issue identifier", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("gh calls = %v, want none for a key outside the configured prefix", runner.calls)
	}
}

func TestGHSourceReviewPRUsesReviewedCommitAndBookmarkFallback(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "review.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var review Row
	if err := json.Unmarshal(fixture, &review); err != nil {
		t.Fatal(err)
	}
	if review.ReviewedSHA == nil || *review.ReviewedSHA != "deadbeef" {
		t.Fatalf("review fixture reviewed_sha = %v, want deadbeef", review.ReviewedSHA)
	}
	runner := &fakeGH{responses: map[string]string{
		"api repos/owner/one/commits/deadbeef/pulls":                                    `[{"number":7}]`,
		"pr list -R owner/one --head fallback --state all --json url,state,headRefName": `[{"url":"https://github.com/owner/one/pull/8","state":"OPEN","headRefName":"fallback"}]`,
		"pr list -R owner/two --head fallback --state all --json url,state,headRefName": `[]`,
		"api repos/owner/one/commits/no-match/pulls":                                    `[]`,
		"api repos/owner/two/commits/no-match/pulls":                                    `[]`,
	}}
	source := NewGHSource(runner, []string{"owner/one", "owner/two"}, "EX", nil)
	ref, err := source.ReviewPR(t.Context(), *review.ReviewedSHA)
	if err != nil {
		t.Fatal(err)
	}
	if ref != "owner/one#7" {
		t.Fatalf("ReviewPR() = %q, want owner/one#7", ref)
	}
	ref, err = source.ReviewPR(t.Context(), "no-match")
	if err != nil {
		t.Fatal(err)
	}
	if ref != "" {
		t.Fatalf("ReviewPR() without a matching commit = %q, want empty", ref)
	}
	fallback, err := source.PRsForBookmark(t.Context(), "fallback")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"owner/one#8"}; !reflect.DeepEqual(fallback, want) {
		t.Fatalf("fallback PRs = %v, want %v", fallback, want)
	}
	wantCalls := [][]string{
		{"api", "repos/owner/one/commits/deadbeef/pulls"},
		{"api", "repos/owner/one/commits/no-match/pulls"},
		{"api", "repos/owner/two/commits/no-match/pulls"},
		{"pr", "list", "-R", "owner/one", "--head", "fallback", "--state", "all", "--json", "url,state,headRefName"},
		{"pr", "list", "-R", "owner/two", "--head", "fallback", "--state", "all", "--json", "url,state,headRefName"},
	}
	if !reflect.DeepEqual(runner.calls, wantCalls) {
		t.Fatalf("gh args = %#v, want %#v", runner.calls, wantCalls)
	}
}

func TestGHSourceFirstPushCIPassUsesFirstCommitEarliestRun(t *testing.T) {
	tests := []struct {
		name     string
		response string
		statuses string
		want     *bool
	}{
		{name: "no runs", response: `[{"total_count":0,"check_runs":[]}]`, statuses: `[{"state":"pending","total_count":0,"statuses":[]}]`, want: nil},
		{name: "checks and statuses return earliest result", response: `[{"total_count":1,"check_runs":[{"id":2,"created_at":"2026-11-03T00:00:00Z","conclusion":"success"}]},{"total_count":1,"check_runs":[{"id":1,"created_at":"2026-11-02T00:00:00Z","conclusion":"failure"}]}]`, statuses: `[{"state":"failure","statuses":[{"created_at":"2026-11-01T00:00:00Z","state":"failure"}]}]`, want: new(false)},
		{name: "earliest check passes", response: `[{"total_count":1,"check_runs":[{"id":1,"created_at":"2026-11-01T00:00:00Z","conclusion":"success"}]},{"total_count":1,"check_runs":[{"id":2,"created_at":"2026-11-02T00:00:00Z","conclusion":"failure"}]}]`, statuses: `[{"state":"failure","statuses":[{"created_at":"2026-11-03T00:00:00Z","state":"failure"}]}]`, want: new(true)},
		{name: "earliest status fails despite combined success", response: `[{"total_count":0,"check_runs":[]}]`, statuses: `[{"state":"success","total_count":2,"statuses":[{"created_at":"2026-11-03T00:00:00Z","state":"success"},{"created_at":"2026-11-01T00:00:00Z","state":"failure"}]}]`, want: new(false)},
		{name: "earliest status passes despite combined failure", response: `[{"total_count":0,"check_runs":[]}]`, statuses: `[{"state":"failure","total_count":2,"statuses":[{"created_at":"2026-11-03T00:00:00Z","state":"failure"},{"created_at":"2026-11-01T00:00:00Z","state":"success"}]}]`, want: new(true)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeGH{responses: map[string]string{
				"api --paginate --slurp repos/owner/repo/pulls/5/commits":              `[[{"sha":"first-sha"}],[{"sha":"later-sha"}]]`,
				"api --paginate --slurp repos/owner/repo/commits/first-sha/check-runs": test.response,
				"api --paginate --slurp repos/owner/repo/commits/first-sha/status":     test.statuses,
			}}
			source := NewGHSource(runner, []string{"owner/repo"}, "EX", nil)
			got, err := source.FirstPushCIPass(t.Context(), "owner/repo#5")
			switch {
			case test.want == nil:
				if !errors.Is(err, errNoCIRuns) || got != nil {
					t.Fatalf("FirstPushCIPass() = %v, %v; want no-CI-runs sentinel", got, err)
				}
			case err != nil:
				t.Fatal(err)
			case got == nil || *got != *test.want:
				t.Fatalf("FirstPushCIPass() = %v, want %v", got, *test.want)
			}
			wantCalls := [][]string{
				{"api", "--paginate", "--slurp", "repos/owner/repo/pulls/5/commits"},
				{"api", "--paginate", "--slurp", "repos/owner/repo/commits/first-sha/check-runs"},
				{"api", "--paginate", "--slurp", "repos/owner/repo/commits/first-sha/status"},
			}
			if !reflect.DeepEqual(runner.calls, wantCalls) {
				t.Fatalf("gh args = %#v, want %#v", runner.calls, wantCalls)
			}
		})
	}
}
func TestGHSourceRevertedMatchesOriginalMergeSHA(t *testing.T) {
	mergedAt := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	runner := &fakeGH{responses: map[string]string{
		"api --paginate --slurp repos/owner/repo/commits?path=&sha=main&since=2026-11-01T00%3A00%3A00Z&until=2026-12-01T00%3A00%3A00Z": `[[{"commit":{"message":"Revert\n\nThis reverts commit 0123456789abcdef0123456789abcdef01234567."},"committer":{"date":"2026-11-02T00:00:00Z"}}]]`,
	}}
	source := NewGHSource(runner, []string{"owner/repo"}, "EX", nil)
	reverted, err := source.Reverted(t.Context(), "owner/repo#3", "0123456789abcdef0123456789abcdef01234567", mergedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !reverted {
		t.Fatal("Reverted() = false, want true")
	}
	wantCalls := [][]string{
		{"api", "--paginate", "--slurp", "repos/owner/repo/commits?path=&sha=main&since=2026-11-01T00%3A00%3A00Z&until=2026-12-01T00%3A00%3A00Z"},
	}
	if !reflect.DeepEqual(runner.calls, wantCalls) {
		t.Fatalf("gh args = %#v, want %#v", runner.calls, wantCalls)
	}
}

func TestGHSourceReviewPRSkipsMissingCommitErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "404", err: errors.New("gh: HTTP 404: Not Found")},
		{name: "no commit found", err: errors.New("gh: No commit found for SHA")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeGH{
				responses: map[string]string{"api repos/owner/two/commits/review-sha/pulls": `[{"number":8}]`},
				errors:    map[string]error{"api repos/owner/one/commits/review-sha/pulls": test.err},
			}
			source := NewGHSource(runner, []string{"owner/one", "owner/two"}, "EX", nil)
			ref, err := source.ReviewPR(t.Context(), "review-sha")
			if err != nil {
				t.Fatal(err)
			}
			if ref != "owner/two#8" {
				t.Fatalf("ReviewPR() = %q, want owner/two#8", ref)
			}
		})
	}
}

func TestGHSourceReviewPRAbortsOnUnexpectedLookupErrorWithoutSHA(t *testing.T) {
	runner := &fakeGH{errors: map[string]error{
		"api repos/owner/one/commits/review-sha/pulls": errors.New("HTTP 500: server error"),
	}}
	source := NewGHSource(runner, []string{"owner/one", "owner/two"}, "EX", nil)
	if _, err := source.ReviewPR(t.Context(), "review-sha"); err == nil {
		t.Fatal("ReviewPR() error = nil, want server error")
	} else if strings.Contains(err.Error(), "review-sha") {
		t.Fatalf("ReviewPR() error exposed row SHA: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("gh calls = %v, want abort after first error", runner.calls)
	}
}

func TestGHSourcePRsForBookmarkRejectsInvalidValuesWithoutLeaking(t *testing.T) {
	runner := &fakeGH{responses: map[string]string{}}
	source := NewGHSource(runner, []string{"owner/repo"}, "EX", nil)
	for _, bookmark := range []string{"--bad", "a" + strings.Repeat("x", 200)} {
		refs, err := source.PRsForBookmark(t.Context(), bookmark)
		if !errors.Is(err, errInvalidBookmark) || len(refs) != 0 || len(runner.calls) != 0 {
			t.Fatalf("PRsForBookmark() = %v, %v, calls = %v; want invalid-bookmark rejection", refs, err, runner.calls)
		}
	}
}

func TestOutcomesIgnoresIssueRowsOutsidePrefix(t *testing.T) {
	inPrefix, outside := "EX-1", "OTHER-1"
	rows := []Row{
		{Issue: &inPrefix, Kind: "message", RowKey: "in"},
		{Issue: &outside, Kind: "message", RowKey: "out"},
	}
	source := &issueRecordingSource{}
	outcomes, err := Outcomes(t.Context(), rows, PriceTable{}, source, "EX")
	if err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != 1 || outcomes[0].Issue.ID != inPrefix {
		t.Fatalf("Outcomes() = %+v, want exactly one outcome for %s", outcomes, inPrefix)
	}
	if want := []string{inPrefix}; !reflect.DeepEqual(source.issueIDs, want) {
		t.Fatalf("IssuePRs() called for %v, want %v; an out-of-prefix key must not reach the source", source.issueIDs, want)
	}
}

// issueRecordingSource records every IssuePRs id and reports no pull requests.
type issueRecordingSource struct {
	issueIDs []string
}

func (source *issueRecordingSource) PR(context.Context, string) (PRState, error) {
	return PRState{}, nil
}

func (*issueRecordingSource) PRsForBookmark(context.Context, string) ([]string, error) {
	return []string{}, nil
}

func (source *issueRecordingSource) IssuePRs(_ context.Context, id string) ([]string, error) {
	source.issueIDs = append(source.issueIDs, id)
	return []string{}, nil
}

func (*issueRecordingSource) FirstPushCIPass(context.Context, string) (*bool, error) {
	return nil, errNoCIRuns
}

func (*issueRecordingSource) Reverted(context.Context, string, string, time.Time) (bool, error) {
	return false, nil
}

func (*issueRecordingSource) ReviewPR(context.Context, string) (string, error) {
	return "", nil
}

func (*issueRecordingSource) Issue(_ context.Context, id string) (IssueState, error) {
	return IssueState{ID: id}, nil
}

func TestOutcomesCachesBookmarkPullRequestsAcrossIssueAndReviewRows(t *testing.T) {
	issue := "EX-42"
	bookmarks := []string{"feature-one", "feature-two"}
	rows := make([]Row, 0, 40)
	for range 10 {
		for _, bookmark := range bookmarks {
			rows = append(rows,
				Row{Issue: &issue, Bookmark: &bookmark, Kind: "message"},
				Row{Issue: &issue, Bookmark: &bookmark, Kind: "review"},
			)
		}
	}
	source := &bookmarkCountingSource{calls: make(map[string]int)}
	_, err := Outcomes(t.Context(), rows, PriceTable{}, source, "EX")
	if err != nil {
		t.Fatal(err)
	}
	for _, bookmark := range bookmarks {
		if got := source.calls[bookmark]; got != 1 {
			t.Errorf("PRsForBookmark(%q) calls = %d, want 1 across issue and review rows", bookmark, got)
		}
	}
}

type bookmarkCountingSource struct {
	calls map[string]int
}

func (source *bookmarkCountingSource) PR(context.Context, string) (PRState, error) {
	return PRState{}, nil
}

func (source *bookmarkCountingSource) PRsForBookmark(_ context.Context, bookmark string) ([]string, error) {
	source.calls[bookmark]++
	ref := "owner/repo#1"
	if bookmark == "feature-two" {
		ref = "owner/repo#2"
	}
	return []string{ref}, nil
}

func (*bookmarkCountingSource) IssuePRs(context.Context, string) ([]string, error) {
	return []string{}, nil
}

func (*bookmarkCountingSource) FirstPushCIPass(context.Context, string) (*bool, error) {
	return nil, errNoCIRuns
}

func (*bookmarkCountingSource) Reverted(context.Context, string, string, time.Time) (bool, error) {
	return false, nil
}

func (*bookmarkCountingSource) ReviewPR(context.Context, string) (string, error) {
	return "", nil
}

func (*bookmarkCountingSource) Issue(_ context.Context, id string) (IssueState, error) {
	return IssueState{ID: id}, nil
}

func TestOutcomesLeavesNoPartialFileOnSourceFailure(t *testing.T) {
	root := t.TempDir()
	issue := "EX-41"
	rows := []Row{{V: 1, Kind: "message", TS: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), RowKey: "r1", Issue: &issue}}
	date := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	if err := WriteOutcomes(t.Context(), root, date, rows, PriceTable{}, failingOutcomeSource{}, "EX"); err == nil {
		t.Fatal("WriteOutcomes() error = nil, want source failure")
	}
	path := filepath.Join(root, "outcomes", "2026-11-09.jsonl")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("output file exists or stat failed after source error: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "outcomes"))
	if err == nil && len(entries) != 0 {
		t.Fatalf("temporary output files remain after source error: %v", entries)
	}
}

type failingOutcomeSource struct{}

func (failingOutcomeSource) PR(context.Context, string) (PRState, error) {
	return PRState{}, errors.New("failure")
}
func (failingOutcomeSource) PRsForBookmark(context.Context, string) ([]string, error) {
	return nil, errors.New("failure")
}
func (failingOutcomeSource) IssuePRs(context.Context, string) ([]string, error) {
	return nil, errors.New("failure")
}
func (failingOutcomeSource) FirstPushCIPass(context.Context, string) (*bool, error) {
	return nil, errors.New("failure")
}
func (failingOutcomeSource) Reverted(context.Context, string, string, time.Time) (bool, error) {
	return false, errors.New("failure")
}
func (failingOutcomeSource) ReviewPR(context.Context, string) (string, error) {
	return "", errors.New("failure")
}
func (failingOutcomeSource) Issue(context.Context, string) (IssueState, error) {
	return IssueState{}, errors.New("failure")
}
