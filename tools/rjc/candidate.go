package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"

	"github.com/RigelBuild/nebula/tools/go-lib/prgates"
	"github.com/samber/oops"
)

type candidate struct{ title, body, path string }
type savedCandidate struct {
	Title    string `json:"title"`
	Authored string `json:"authored"`
	Issues   []int  `json:"issues,omitempty"`
}

func (r *repository) template(ctx context.Context) (string, error) {
	files, err := r.jj(ctx, "file", "list", "-r", r.profile.TrunkRemote())
	if err != nil {
		return "", err
	}
	paths := strings.Split(files, "\n")
	for _, path := range []string{".github/PULL_REQUEST_TEMPLATE.md", ".github/pull_request_template.md", "PULL_REQUEST_TEMPLATE.md", "pull_request_template.md", "docs/PULL_REQUEST_TEMPLATE.md", "docs/pull_request_template.md"} {
		if slices.Contains(paths, path) {
			return r.jj(ctx, "file", "show", "-r", r.profile.TrunkRemote(), "root:"+path)
		}
	}
	for _, file := range paths {
		if strings.HasPrefix(strings.ToLower(file), ".github/pull_request_template/") {
			return "", refusal("metadata", "ambiguous-template", "repository has named templates without a selected applicable template", "select the documented template before submitting")
		}
	}
	return fallbackTemplate, nil
}

func (r *repository) prepareResolvedCandidate(ctx context.Context, bookmark, selectionTip, title, authored, template, skip string) (candidate, error) {
	if !r.keys.Owned(r.codename, bookmark) {
		return candidate{}, refusal("metadata", "ownership", "bookmark is not owned by the caller", "use the caller's codename bookmark")
	}
	if selectionTip == "" {
		selectionTip = bookmark
	}
	if _, err := r.one(ctx, quoteRev(selectionTip)); err != nil {
		return candidate{}, err
	}
	issues, err := r.loadIssues(bookmark)
	if err != nil {
		return candidate{}, err
	}
	title, err = r.keys.FullTitle(title, issues)
	if err != nil {
		return candidate{}, err
	}
	body, err := r.composeCandidateWithTemplate(bookmark, authored, skip, template)
	if err != nil {
		return candidate{}, err
	}
	result := prgates.MetadataBounds(prgates.BoundsFacts{Title: title, Body: body, IssuePrefix: r.keys.prefix})
	if !result.Pass {
		return candidate{}, refusal("metadata", result.Gate, result.Message, "edit --title or --body-file to meet the bounds")
	}
	return candidate{title: title, body: body}, nil
}

func (r *repository) saveCandidate(ctx context.Context, bookmark, title, bodyFile string, flags []namedSection) error {
	template, err := r.template(ctx)
	if err != nil {
		return err
	}
	authored, err := metadataBody(template, bodyFile, flags)
	if err != nil {
		return err
	}
	c, err := r.prepareResolvedCandidate(ctx, bookmark, "", title, authored, template, "")
	if err != nil {
		return err
	}
	store := fileBaseline{root: r.stateDir, keys: r.keys}
	path, err := store.path(bookmark, ".metadata.json")
	if err != nil {
		return err
	}
	issues, err := r.loadIssues(bookmark)
	if err != nil {
		return err
	}
	data, err := json.Marshal(savedCandidate{Title: c.title, Authored: c.body, Issues: issues})
	if err != nil {
		return oops.Wrapf(err, "encode saved metadata")
	}
	return atomicWrite(path, string(data))
}

func (r *repository) composeCandidate(ctx context.Context, bookmark, authored, skip string) (string, error) {
	template, err := r.template(ctx)
	if err != nil {
		return "", err
	}
	return r.composeCandidateWithTemplate(bookmark, authored, skip, template)
}

func (r *repository) composeCandidateWithTemplate(bookmark, authored, skip, template string) (string, error) {
	issues, err := r.loadIssues(bookmark)
	if err != nil {
		return "", err
	}
	return r.composeAuthoredBody(template, authored, skip, issues)
}

// skipMarkdown renders a reason as a code span so Markdown does not interpret it.
func skipMarkdown(reason string) string {
	longestRun, run := 0, 0
	for _, character := range reason {
		if character == '`' {
			run++
			if run > longestRun {
				longestRun = run
			}
			continue
		}
		run = 0
	}

	fenceLength := longestRun + 1
	padding := 0
	if strings.HasPrefix(reason, "`") || strings.HasSuffix(reason, "`") {
		padding = 1
	}
	var rendered strings.Builder
	rendered.Grow(len(reason) + 2*fenceLength + 2*padding)
	for range fenceLength {
		rendered.WriteByte('`')
	}
	if padding != 0 {
		rendered.WriteByte(' ')
	}
	rendered.WriteString(reason)
	if padding != 0 {
		rendered.WriteByte(' ')
	}
	for range fenceLength {
		rendered.WriteByte('`')
	}
	return rendered.String()
}

func (r *repository) composeAuthoredBody(template, authored, skip string, issues []int) (string, error) {
	authored = skippedChecksRE.ReplaceAllString(authored, "")
	marker := "rjc-generated-skip-reason-marker"
	if skip != "" {
		authored = strings.TrimSpace(r.keys.trailingRE.ReplaceAllString(authored, ""))
		for strings.Contains(authored, marker) || strings.Contains(template, marker) {
			marker += "-"
		}
		authored += "\n\nChecks skipped: " + marker + "\n"
	}
	body, err := ComposeBody(r.keys, template, authored, issues, r.profile.CoAuthor)
	if err != nil {
		return "", refusal("metadata", "body-template", err.Error(), "fill the repository PR template in --body-file")
	}
	if skip != "" {
		body = strings.Replace(body, "Checks skipped: "+marker, "Checks skipped: "+skipMarkdown(skip), 1)
	}
	return body, nil
}

func (r *repository) loadCandidate(bookmark string) (candidate, error) {
	store := fileBaseline{root: r.stateDir, keys: r.keys}
	path, err := store.path(bookmark, ".metadata.json")
	if err != nil {
		return candidate{}, err
	}
	data, err := os.ReadFile(path) //nolint:gosec // fileBaseline.path validates the owned bookmark before selecting state.
	if errors.Is(err, os.ErrNotExist) {
		return candidate{}, refusal("metadata", "missing-saved-candidate", "lower bookmark has no saved metadata candidate: "+bookmark, "rjc meta "+bookmark+` --title "<title>" --body-file <path>`)
	}
	if err != nil {
		return candidate{}, oops.Wrapf(err, "read saved metadata candidate")
	}
	var saved savedCandidate
	if err := json.Unmarshal(data, &saved); err != nil {
		return candidate{}, oops.Wrapf(err, "decode saved metadata candidate")
	}
	if saved.Title == "" || saved.Authored == "" {
		return candidate{}, refusal("metadata", "missing-saved-candidate", "lower bookmark has no saved metadata candidate: "+bookmark, "rjc meta "+bookmark+` --title "<title>" --body-file <path>`)
	}
	return candidate{title: saved.Title, body: saved.Authored}, nil
}

func (r *repository) checkCandidate(ctx context.Context, change Change, c candidate, skip string) error {
	issues, err := r.loadIssues(change.Bookmark)
	if err != nil {
		return err
	}
	template, err := r.template(ctx)
	if err != nil {
		return err
	}
	return r.checkCandidateWithInputs(ctx, change, c, skip, template, issues)
}

func (r *repository) checkCandidateWithInputs(ctx context.Context, change Change, c candidate, skip, template string, issues []int) error {
	expected, err := r.keys.FullTitle(c.title, issues)
	if err != nil {
		return err
	}
	if expected != c.title {
		return refusal("metadata", "issue-id", "saved title must end with the bookmark's issue", "rjc meta "+change.Bookmark+` --title "<title>" --body-file <path>`)
	}
	canonical, err := r.composeAuthoredBody(template, c.body, skip, issues)
	if err != nil {
		return err
	}
	if canonical != c.body {
		return refusal("metadata", "saved-body-template", "metadata no longer matches the current repository template and footer", "rjc submit "+change.Bookmark+" with an updated body before its dependents")
	}
	fork := "heads(::" + quoteRev(change.Base) + " & ::" + quoteRev(change.Tip) + ")"
	files, err := r.jj(ctx, "diff", "--name-only", "--from", fork, "--to", quoteRev(change.Tip))
	if err != nil {
		return err
	}
	changed := []string{}
	for file := range strings.SplitSeq(files, "\n") {
		if file == "" {
			continue
		}
		changed = append(changed, file)
	}
	for _, gate := range r.profile.CIGates {
		var result prgates.Result
		switch gate {
		case "branch-name":
			result = r.keys.branchName(change.Bookmark)
		case "pr-title-issue-ref":
			result = prgates.TitleIssueRef(prgates.TitleFacts{Title: c.title, IssuePrefix: r.keys.prefix})
		case specImpactGate:
			result = prgates.SpecImpact(prgates.SpecImpactFacts{ChangedFiles: changed, Body: c.body, HeadBranch: change.Bookmark, SpecDirs: r.profile.SpecDirs})
		case metadataBoundsGate:
			result = prgates.MetadataBounds(prgates.BoundsFacts{Title: c.title, Body: c.body, IssuePrefix: r.keys.prefix})
		default:
			return oops.Errorf("unsupported CI gate %q", gate)
		}
		if !result.Pass {
			return refusal("metadata", gate, result.Message, "edit the candidate for "+change.Bookmark+" and rerun rjc submit")
		}
	}
	return nil
}

func (r *repository) loadIssues(bookmark string) ([]int, error) {
	primary, err := r.keys.bookmarkIssue(bookmark)
	if err != nil {
		return nil, err
	}
	path, err := (fileBaseline{root: r.stateDir, keys: r.keys}).path(bookmark, ".metadata.json")
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path) //nolint:gosec // path validates the bookmark.
	if errors.Is(err, os.ErrNotExist) {
		return []int{primary}, nil
	}
	if err != nil {
		return nil, oops.Wrapf(err, "read saved issues")
	}
	var saved savedCandidate
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil, oops.Wrapf(err, "decode saved issues")
	}
	if len(saved.Issues) == 0 {
		return []int{primary}, nil
	}
	if saved.Issues[0] != primary {
		return nil, refusal("metadata", "issue-id", "saved primary issue differs from bookmark", "repair saved metadata")
	}
	return saved.Issues, nil
}

func (r *repository) saveIssues(bookmark string, issues []int) error {
	path, err := (fileBaseline{root: r.stateDir, keys: r.keys}).path(bookmark, ".metadata.json")
	if err != nil {
		return err
	}
	data, err := json.Marshal(savedCandidate{Issues: issues})
	if err != nil {
		return oops.Wrapf(err, "encode saved issues")
	}
	return atomicWrite(path, string(data))
}
