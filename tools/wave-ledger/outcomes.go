package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec" //nolint:depguard // The tool invokes gh directly with argv, without a shell.
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/issueowner"
)

var (
	repoPattern        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9_.-]+$`)
	prefixPattern      = regexp.MustCompile(`^[A-Z][A-Z0-9]{0,15}$`)
	issueIDPattern     = regexp.MustCompile(`^([A-Z][A-Z0-9]{0,15})-([1-9][0-9]*)$`)
	prPattern          = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9-]*)/([A-Za-z0-9_.-]+)#([1-9][0-9]*)$`)
	prURLPattern       = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9][A-Za-z0-9-]*)/([A-Za-z0-9_.-]+)/pull/([1-9][0-9]*)/?$`)
	revertRE           = regexp.MustCompile(`(?i)this reverts commit ([0-9a-f]{7,40})\b`)
	bookmarkPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_./-]{0,199}$`)
	errNoCIRuns        = errors.New("wave-ledger: no ci runs")
	errInvalidBookmark = errors.New("wave-ledger: invalid bookmark")
	errInvalidIssue    = errors.New("wave-ledger: invalid issue identifier")
)

type PRState struct {
	Ref               string     `json:"ref"`
	State             string     `json:"state"`
	MergeSHA          string     `json:"merge_sha"`
	MergedAt          *time.Time `json:"merged_at"`
	FirstPushCIPass   *bool      `json:"first_push_ci_pass"`
	RevertedWithin30d *bool      `json:"reverted_within_30d"`
}

type IssueState struct {
	ID          string     `json:"id"`
	StateType   string     `json:"state_type"`
	CompletedAt *time.Time `json:"completed_at"`
}

type OutcomeSource interface {
	PR(ctx context.Context, ref string) (PRState, error)
	PRsForBookmark(ctx context.Context, bookmark string) ([]string, error)
	IssuePRs(ctx context.Context, id string) ([]string, error)
	FirstPushCIPass(ctx context.Context, ref string) (*bool, error)
	Reverted(ctx context.Context, ref string, mergeSHA string, mergedAt time.Time) (bool, error)
	ReviewPR(ctx context.Context, sha string) (string, error)
	Issue(ctx context.Context, id string) (IssueState, error)
}

type Outcome struct {
	Issue          IssueState         `json:"issue"`
	PRs            []PRState          `json:"prs"`
	ReviewRounds   int                `json:"review_rounds"`
	FindingsHigh   int                `json:"findings_high"`
	FindingsMedium int                `json:"findings_medium"`
	FindingsLow    int                `json:"findings_low"`
	Calls          int                `json:"calls"`
	CostUSD        float64            `json:"cost_usd"`
	ByAgent        map[string]float64 `json:"by_agent"`
}

type GHRunner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

type execGHRunner struct{}

func (execGHRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "gh", args...) //nolint:gosec // The executable is fixed and arguments bypass shell parsing.
	output, err := command.Output()
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return nil, &ghCommandError{cause: err, stderr: string(exitErr.Stderr)}
		}
		return nil, fmt.Errorf("running gh command: %w", err)
	}
	return output, nil
}

type ghCommandError struct {
	cause  error
	stderr string
}

func (err *ghCommandError) Error() string { return err.stderr }
func (err *ghCommandError) Unwrap() error { return err.cause }

type GHSource struct {
	runner      GHRunner
	repos       []string
	issuePrefix string
	linear      issueowner.LinearIssueAPI
}

func NewGHSource(runner GHRunner, repos []string, issuePrefix string, linear issueowner.LinearIssueAPI) *GHSource {
	return &GHSource{runner: runner, repos: append([]string(nil), repos...), issuePrefix: issuePrefix, linear: linear}
}

func (source *GHSource) PR(ctx context.Context, ref string) (PRState, error) {
	parts, err := parsePRRef(ref)
	if err != nil {
		return PRState{}, err
	}
	data, err := source.run(ctx, "pr", "view", parts.number, "-R", parts.repo, "--json", "state,mergedAt,mergeCommit")
	if err != nil {
		return PRState{}, fmt.Errorf("reading pull request: %w", err)
	}
	var response struct {
		State       string     `json:"state"`
		MergedAt    *time.Time `json:"mergedAt"`
		MergeCommit struct {
			OID string `json:"oid"`
		} `json:"mergeCommit"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return PRState{}, fmt.Errorf("decoding pull request: %w", err)
	}
	return PRState{Ref: ref, State: strings.ToLower(response.State), MergeSHA: response.MergeCommit.OID, MergedAt: response.MergedAt}, nil
}

func (source *GHSource) PRsForBookmark(ctx context.Context, bookmark string) ([]string, error) {
	if !bookmarkPattern.MatchString(bookmark) {
		return nil, errInvalidBookmark
	}
	refs := make([]string, 0)
	for _, repo := range source.repos {
		data, err := source.run(ctx, "pr", "list", "-R", repo, "--head", bookmark, "--state", "all", "--json", "url,state,headRefName")
		if err != nil {
			return nil, fmt.Errorf("listing pull requests in repository %s: %w", repo, err)
		}
		var prs []struct {
			URL         string `json:"url"`
			HeadRefName string `json:"headRefName"`
		}
		if err := json.Unmarshal(data, &prs); err != nil {
			return nil, fmt.Errorf("decoding pull requests in repository %s: %w", repo, err)
		}
		for _, pr := range prs {
			if pr.HeadRefName != bookmark {
				continue
			}
			if ref, ok := prRefFromURL(pr.URL); ok {
				refs = append(refs, ref)
			}
		}
	}
	return uniqueSorted(refs), nil
}

func (source *GHSource) IssuePRs(ctx context.Context, id string) ([]string, error) {
	match := issueIDPattern.FindStringSubmatch(id)
	if match == nil || match[1] != source.issuePrefix {
		return nil, errInvalidIssue
	}
	trailer := regexp.MustCompile(`(?im)^Refs\s+` + regexp.QuoteMeta(id) + `\s*$`)
	bookmark := regexp.MustCompile(`(?i)(?:^|/)` + regexp.QuoteMeta(strings.ToLower(match[1])) + `-` + match[2] + `-`)
	refs := make([]string, 0)
	for _, repo := range source.repos {
		data, err := source.run(ctx, "api", "--paginate", "--slurp", "repos/"+repo+"/pulls?state=all&per_page=100")
		if err != nil {
			return nil, fmt.Errorf("listing pull requests in %s: %w", repo, err)
		}
		var pages [][]struct {
			Number int `json:"number"`
			Head   struct {
				Ref string `json:"ref"`
			} `json:"head"`
			Body string `json:"body"`
		}
		if err := json.Unmarshal(data, &pages); err != nil {
			return nil, fmt.Errorf("decoding pull requests in %s: %w", repo, err)
		}
		for _, page := range pages {
			for _, pr := range page {
				if !bookmark.MatchString(pr.Head.Ref) && !trailer.MatchString(pr.Body) {
					continue
				}
				refs = append(refs, fmt.Sprintf("%s#%d", repo, pr.Number))
			}
		}
	}
	return uniqueSorted(refs), nil
}

func (source *GHSource) FirstPushCIPass(ctx context.Context, ref string) (*bool, error) {
	parts, err := parsePRRef(ref)
	if err != nil {
		return nil, err
	}
	commitData, err := source.run(ctx, "api", "--paginate", "--slurp", "repos/"+parts.repo+"/pulls/"+parts.number+"/commits")
	if err != nil {
		return nil, fmt.Errorf("reading commits for pull request: %w", err)
	}
	var commitPages [][]struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(commitData, &commitPages); err != nil {
		return nil, fmt.Errorf("decoding commits for pull request: %w", err)
	}
	if len(commitPages) == 0 || len(commitPages[0]) == 0 || commitPages[0][0].SHA == "" {
		return nil, errNoCIRuns
	}
	commitPath := "repos/" + parts.repo + "/commits/" + url.PathEscape(commitPages[0][0].SHA)
	checkData, err := source.run(ctx, "api", "--paginate", "--slurp", commitPath+"/check-runs")
	if err != nil {
		return nil, fmt.Errorf("reading checks for pull request: %w", err)
	}
	var checkPages []struct {
		CheckRuns []struct {
			CreatedAt  time.Time `json:"created_at"`
			Conclusion *string   `json:"conclusion"`
		} `json:"check_runs"`
	}
	if err := json.Unmarshal(checkData, &checkPages); err != nil {
		return nil, fmt.Errorf("decoding checks for pull request: %w", err)
	}
	statusData, err := source.run(ctx, "api", "--paginate", "--slurp", commitPath+"/status")
	if err != nil {
		return nil, fmt.Errorf("reading statuses for pull request: %w", err)
	}
	var statusPages []struct {
		State    string `json:"state"`
		Statuses []struct {
			CreatedAt time.Time `json:"created_at"`
			State     string    `json:"state"`
		} `json:"statuses"`
	}
	if err := json.Unmarshal(statusData, &statusPages); err != nil {
		return nil, fmt.Errorf("decoding statuses for pull request: %w", err)
	}
	type result struct {
		created time.Time
		passed  bool
	}
	runs := make([]result, 0)
	for _, page := range checkPages {
		for _, check := range page.CheckRuns {
			if !check.CreatedAt.IsZero() {
				runs = append(runs, result{created: check.CreatedAt, passed: check.Conclusion != nil && *check.Conclusion == "success"})
			}
		}
	}
	for _, page := range statusPages {
		if len(page.Statuses) == 0 {
			continue
		}
		first := page.Statuses[0]
		for _, status := range page.Statuses[1:] {
			if !status.CreatedAt.IsZero() && (first.CreatedAt.IsZero() || status.CreatedAt.Before(first.CreatedAt)) {
				first = status
			}
		}
		if !first.CreatedAt.IsZero() {
			runs = append(runs, result{created: first.CreatedAt, passed: first.State == "success"})
		}
	}
	if len(runs) == 0 {
		return nil, errNoCIRuns
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].created.Before(runs[j].created) })
	passed := runs[0].passed
	return &passed, nil
}

func (source *GHSource) Reverted(ctx context.Context, ref string, mergeSHA string, mergedAt time.Time) (bool, error) {
	parts, err := parsePRRef(ref)
	if err != nil {
		return false, err
	}
	mergedAt = mergedAt.UTC()
	start := mergedAt.Format(time.RFC3339)
	endAt := mergedAt.AddDate(0, 0, 30)
	query := "repos/" + parts.repo + "/commits?path=&sha=main&since=" + url.QueryEscape(start) + "&until=" + url.QueryEscape(endAt.Format(time.RFC3339))
	commits, err := source.run(ctx, "api", "--paginate", "--slurp", query)
	if err != nil {
		return false, fmt.Errorf("listing commits for reverts: %w", err)
	}
	var pages [][]struct {
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
		Committer struct {
			Date time.Time `json:"date"`
		} `json:"committer"`
	}
	if err := json.Unmarshal(commits, &pages); err != nil {
		return false, fmt.Errorf("decoding commits for reverts: %w", err)
	}
	for _, page := range pages {
		for _, entry := range page {
			match := revertRE.FindStringSubmatch(entry.Commit.Message)
			if match == nil || !strings.EqualFold(match[1], mergeSHA) || entry.Committer.Date.Before(mergedAt) || entry.Committer.Date.After(endAt) {
				continue
			}
			return true, nil

		}
	}
	return false, nil

}

func (source *GHSource) ReviewPR(ctx context.Context, sha string) (string, error) {
	if sha == "" {
		return "", nil
	}
	for _, repo := range source.repos {
		data, err := source.run(ctx, "api", "repos/"+repo+"/commits/"+url.PathEscape(sha)+"/pulls")
		if err != nil {
			message := strings.ToLower(err.Error())
			if strings.Contains(message, "http 404") || strings.Contains(message, "no commit found for sha") {
				continue
			}
			return "", fmt.Errorf("looking up reviewed commit in %s: %w", repo, err)
		}

		var prs []struct {
			Number         int        `json:"number"`
			State          string     `json:"state"`
			MergedAt       *time.Time `json:"merged_at"`
			MergeCommitSHA string     `json:"merge_commit_sha"`
			Head           struct {
				Repo *struct {
					FullName string `json:"full_name"`
				} `json:"repo"`
			} `json:"head"`
		}
		if err := json.Unmarshal(data, &prs); err != nil {
			return "", errors.New("decoding pull requests for reviewed commit")
		}
		for _, pr := range prs {
			if pr.Number > 0 {
				return fmt.Sprintf("%s#%d", repo, pr.Number), nil
			}
		}
	}
	return "", nil
}

func (source *GHSource) Issue(ctx context.Context, id string) (IssueState, error) {
	if source.linear == nil {
		return IssueState{}, errors.New("linear issue api is nil")
	}
	fields, err := source.linear.FetchIssue(ctx, id)
	if err != nil {
		return IssueState{}, fmt.Errorf("fetching Linear issue: %w", err)
	}
	state := IssueState{ID: fields.Identifier, StateType: fields.StateType}
	if fields.CompletedAt != nil && *fields.CompletedAt != "" {
		completedAt, err := time.Parse(time.RFC3339Nano, *fields.CompletedAt)
		if err != nil {
			return IssueState{}, fmt.Errorf("parsing issue completion time: %w", err)
		}
		state.CompletedAt = &completedAt
	}
	return state, nil
}
func (source *GHSource) run(ctx context.Context, args ...string) ([]byte, error) {
	if source.runner == nil {
		return nil, errors.New("github command runner is nil")
	}
	return source.runner.Run(ctx, args...)
}

type prRef struct {
	repo, number string
}

func parsePRRef(ref string) (prRef, error) {
	match := prPattern.FindStringSubmatch(ref)
	if match == nil {
		return prRef{}, fmt.Errorf("invalid pull request reference %q", ref)
	}
	return prRef{repo: match[1] + "/" + match[2], number: match[3]}, nil
}

func prRefFromURL(value string) (string, bool) {
	match := prURLPattern.FindStringSubmatch(value)
	if match == nil {
		return "", false
	}
	return match[1] + "/" + match[2] + "#" + match[3], true
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

type bookmarkPRResult struct {
	refs []string
	err  error
}

type cachedOutcomeSource struct {
	OutcomeSource
	bookmarkPRs map[string]bookmarkPRResult
}

func newCachedOutcomeSource(source OutcomeSource) *cachedOutcomeSource {
	return &cachedOutcomeSource{
		OutcomeSource: source,
		bookmarkPRs:   make(map[string]bookmarkPRResult),
	}
}

func (source *cachedOutcomeSource) PRsForBookmark(ctx context.Context, bookmark string) ([]string, error) {
	if result, ok := source.bookmarkPRs[bookmark]; ok {
		return result.refs, result.err
	}
	refs, err := source.OutcomeSource.PRsForBookmark(ctx, bookmark)
	source.bookmarkPRs[bookmark] = bookmarkPRResult{refs: refs, err: err}
	return refs, err
}

func Outcomes(ctx context.Context, rows []Row, table PriceTable, source OutcomeSource, issuePrefix string) ([]Outcome, error) {
	if source == nil {
		return nil, errors.New("outcome source is nil")
	}
	source = newCachedOutcomeSource(source)
	issues := groupIssueRows(rows, issuePrefix)
	ids := make([]string, 0, len(issues))
	for id := range issues {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	outcomes := make([]Outcome, 0, len(ids))
	for _, id := range ids {
		outcome, err := outcomeForIssue(ctx, id, issues[id], table, source)
		if err != nil {
			return nil, err
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes, nil
}

func groupIssueRows(rows []Row, issuePrefix string) map[string][]Row {
	issues := make(map[string][]Row)
	for _, row := range rows {
		if row.Issue == nil {
			continue
		}
		match := issueIDPattern.FindStringSubmatch(*row.Issue)
		if match == nil || match[1] != issuePrefix {
			continue
		}
		issues[*row.Issue] = append(issues[*row.Issue], row)
	}
	return issues
}

func outcomeForIssue(ctx context.Context, id string, rows []Row, table PriceTable, source OutcomeSource) (Outcome, error) {
	issue, err := source.Issue(ctx, id)
	if err != nil {
		return Outcome{}, err
	}
	refs, err := issuePRRefs(ctx, id, rows, source)
	if err != nil {
		return Outcome{}, err
	}
	prSet := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		prSet[ref] = struct{}{}
	}
	prStates, err := loadPRStates(ctx, refs, source)
	if err != nil {
		return Outcome{}, err
	}
	outcome := Outcome{Issue: issue, PRs: prStates, ByAgent: make(map[string]float64)}
	if err := addReviewOutcomes(ctx, &outcome, rows, prSet, source); err != nil {
		return Outcome{}, err
	}
	addUsageOutcomes(&outcome, rows, table)
	return outcome, nil
}

func issuePRRefs(ctx context.Context, id string, rows []Row, source OutcomeSource) ([]string, error) {
	prSet := make(map[string]struct{})
	bookmarks := make(map[string]struct{})
	for _, row := range rows {
		if row.Bookmark == nil || *row.Bookmark == "" {
			continue
		}
		if _, ok := bookmarks[*row.Bookmark]; ok {
			continue
		}
		bookmarks[*row.Bookmark] = struct{}{}
		refs, err := source.PRsForBookmark(ctx, *row.Bookmark)
		if errors.Is(err, errInvalidBookmark) {
			continue
		}
		if err != nil {
			return nil, err
		}
		addRefs(prSet, refs)
	}
	for _, row := range rows {
		if row.PR == nil {
			continue
		}
		if _, err := parsePRRef(*row.PR); err == nil {
			prSet[*row.PR] = struct{}{}
		}
	}
	refs, err := source.IssuePRs(ctx, id)
	if err != nil {
		return nil, err
	}
	addRefs(prSet, refs)
	result := make([]string, 0, len(prSet))
	for ref := range prSet {
		result = append(result, ref)
	}
	sort.Strings(result)
	return result, nil
}

func loadPRStates(ctx context.Context, refs []string, source OutcomeSource) ([]PRState, error) {
	states := make([]PRState, 0, len(refs))
	for _, ref := range refs {
		state, err := source.PR(ctx, ref)
		if err != nil {
			return nil, err
		}
		firstPass, err := source.FirstPushCIPass(ctx, ref)
		if errors.Is(err, errNoCIRuns) {
			firstPass, err = nil, nil
		}
		if err != nil {
			return nil, err
		}
		state.FirstPushCIPass = firstPass
		if state.MergedAt != nil && state.MergeSHA != "" {
			reverted, err := source.Reverted(ctx, ref, state.MergeSHA, *state.MergedAt)
			if err != nil {
				return nil, err
			}
			state.RevertedWithin30d = &reverted
		}
		states = append(states, state)
	}
	return states, nil
}

func addReviewOutcomes(ctx context.Context, outcome *Outcome, rows []Row, prSet map[string]struct{}, source OutcomeSource) error {
	for _, row := range rows {
		if row.Kind != "review" {
			continue
		}
		ref, err := reviewPRRef(ctx, row, source)
		if err != nil {
			return err
		}
		if _, matched := prSet[ref]; !matched {
			continue
		}
		outcome.ReviewRounds++
		outcome.FindingsHigh += intValue(row.FindingsHigh)
		outcome.FindingsMedium += intValue(row.FindingsMedium)
		outcome.FindingsLow += intValue(row.FindingsLow)
	}
	return nil
}

func reviewPRRef(ctx context.Context, row Row, source OutcomeSource) (string, error) {
	if row.ReviewedSHA != nil && *row.ReviewedSHA != "" {
		ref, err := source.ReviewPR(ctx, *row.ReviewedSHA)
		if err != nil {
			return "", err
		}
		if ref != "" {
			return ref, nil
		}
	}
	if row.Bookmark == nil || *row.Bookmark == "" {
		return "", nil
	}
	refs, err := source.PRsForBookmark(ctx, *row.Bookmark)
	if errors.Is(err, errInvalidBookmark) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if len(refs) == 0 {
		return "", nil
	}
	return refs[0], nil
}

func addUsageOutcomes(outcome *Outcome, rows []Row, table PriceTable) {
	for _, row := range rows {
		if row.Kind != kindMessage && row.Kind != kindModelUsage {
			continue
		}
		outcome.Calls++
		cost, _ := Reprice(row, table)
		if cost == nil {
			continue
		}
		outcome.CostUSD += *cost
		if row.Agent != nil {
			outcome.ByAgent[*row.Agent] += *cost
		}
	}
}

func addRefs(target map[string]struct{}, values []string) {
	for _, value := range values {
		if _, err := parsePRRef(value); err == nil {
			target[value] = struct{}{}
		}
	}
}

func intValue(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func WriteOutcomes(ctx context.Context, dir string, date time.Time, rows []Row, table PriceTable, source OutcomeSource, issuePrefix string) error {
	outcomes, err := Outcomes(ctx, rows, table, source, issuePrefix)
	if err != nil {
		return err
	}
	outDir := filepath.Join(dir, "outcomes")
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return fmt.Errorf("creating outcomes directory: %w", err)
	}
	path := filepath.Join(outDir, utcDay(date).Format("2006-01-02")+".jsonl")
	file, err := os.CreateTemp(outDir, ".outcomes-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temporary outcomes file: %w", err)
	}
	tempPath := file.Name()
	defer func() {
		if err := os.Remove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Error("removing temporary outcomes file failed", "error", err)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return closeFileWithError(file, fmt.Errorf("setting outcomes file mode: %w", err))
	}
	writer := bytes.NewBuffer(nil)
	for _, outcome := range outcomes {
		line, err := json.Marshal(outcome)
		if err != nil {
			return closeFileWithError(file, fmt.Errorf("encoding outcome: %w", err))
		}
		writer.Write(line)
		writer.WriteByte('\n')
	}
	if _, err := file.Write(writer.Bytes()); err != nil {
		return closeFileWithError(file, fmt.Errorf("writing outcomes file: %w", err))
	}
	if err := file.Sync(); err != nil {
		return closeFileWithError(file, fmt.Errorf("syncing outcomes file: %w", err))
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing outcomes file: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("publishing outcomes file: %w", err)
	}
	return nil
}

func closeFileWithError(file *os.File, cause error) error {
	if err := file.Close(); err != nil {
		return fmt.Errorf("%w: closing temporary file: %w", cause, err)
	}
	return cause
}
