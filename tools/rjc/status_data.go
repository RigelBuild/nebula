package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
)

type statusOptions struct {
	repo string
	all  bool
}

type statusPR struct {
	State       string `json:"state"`
	IsDraft     bool   `json:"isDraft"`
	Mergeable   string `json:"mergeable"`
	HeadRefOid  string `json:"headRefOid"`
	BaseRefName string `json:"baseRefName"`
	Comments    struct {
		Nodes []struct {
			Body   string
			Url    string
			Author struct{ Login string }
		}
		PageInfo struct{ HasNextPage bool }
	}
	TimelineItems struct {
		Nodes []struct {
			Typename   string `json:"__typename"`
			DatabaseId int64
			State      string
			Review     struct{ DatabaseId int64 }
		}
		PageInfo struct{ HasNextPage bool }
	}
	Commits struct {
		Nodes []struct {
			Commit struct {
				Tree struct {
					Entries []struct {
						Name   string
						Object struct{ Oid string }
					}
				}
				StatusCheckRollup struct {
					Contexts struct {
						Nodes []struct {
							Typename                                 string `json:"__typename"`
							Name, Status, Conclusion, Context, State string
						}
						PageInfo struct{ HasNextPage bool }
					}
				}
			}
		}
	}
}

func statusQuery(entries []LedgerEntry) string {
	var b strings.Builder
	b.WriteString(`query($owner:String!,$name:String!){viewer{login} repository(owner:$owner,name:$name){`)
	for i, e := range entries {
		fmt.Fprintf(&b, `p%d:pullRequest(number:%d){state isDraft mergeable headRefOid baseRefName comments(first:100){pageInfo{hasNextPage} nodes{body url author{login}}} timelineItems(first:100,itemTypes:[PULL_REQUEST_COMMIT,HEAD_REF_FORCE_PUSHED_EVENT,PULL_REQUEST_REVIEW,REVIEW_DISMISSED_EVENT]){pageInfo{hasNextPage} nodes{__typename ... on PullRequestReview{databaseId state} ... on ReviewDismissedEvent{review{databaseId}}}} commits(last:1){nodes{commit{tree{entries{name object{oid}}} statusCheckRollup{contexts(first:100){pageInfo{hasNextPage} nodes{__typename ... on CheckRun{name status conclusion} ... on StatusContext{context state}}}}}}}}`, i, e.PR)
	}
	b.WriteString(`}}`)
	return b.String()
}

// statusChunkSize caps the pull requests in one status query so GitHub answers before its request timeout.
const statusChunkSize = 25

// statusPullRequests fetches a repository's pull requests in chunks, one query at a time, in entry order.
func statusPullRequests(ctx context.Context, forge *forgeClient, repo string, group []LedgerEntry) (string, []statusPR, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return "", nil, errors.New("invalid ledger repository")
	}
	viewer := ""
	prs := make([]statusPR, 0, len(group))
	for start := 0; start < len(group); start += statusChunkSize {
		chunk := group[start:min(start+statusChunkSize, len(group))]
		var response struct {
			Data struct {
				Viewer     struct{ Login string }
				Repository map[string]json.RawMessage
			}
			Errors []struct{ Message string }
		}
		payload := map[string]any{graphqlQueryKey: statusQuery(chunk), graphqlVariablesKey: map[string]any{"owner": owner, "name": name}}
		if err := forge.api(ctx, http.MethodPost, "/graphql", payload, &response); err != nil {
			return "", nil, err
		}
		if len(response.Errors) > 0 {
			return "", nil, fmt.Errorf("status query: %s", response.Errors[0].Message)
		}
		viewer = response.Data.Viewer.Login
		for i, e := range chunk {
			var pr statusPR
			data := response.Data.Repository[fmt.Sprintf("p%d", i)]
			if len(data) == 0 || string(data) == "null" {
				return "", nil, fmt.Errorf("PR %s#%d missing", repo, e.PR)
			}
			if err := json.Unmarshal(data, &pr); err != nil { //nolint:musttag // GraphQL names map case-insensitively to Go fields.
				return "", nil, err
			}
			prs = append(prs, pr)
		}
	}
	return viewer, prs, nil
}

func statusEvidenceCurrent(rec, identity HookRecord) bool {
	return rec.Current(identity.Repo, identity.Head, identity.Base, identity.HookConfigBlob)
}

func (r *repository) status(ctx context.Context, opt statusOptions) ([]StatusRow, error) {
	entries, err := (Ledger{root: r.stateDir}).List(r.keys, r.codename, opt.repo)
	if err != nil {
		return nil, err
	}
	groups := map[string][]LedgerEntry{}
	for _, e := range entries {
		groups[e.Repo] = append(groups[e.Repo], e)
	}
	repos := make([]string, 0, len(groups))
	for repo := range groups {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	rows := []StatusRow{}
	s, err := service(ctx)
	if err != nil {
		return nil, err
	}
	for _, repo := range repos {
		group := groups[repo]
		viewer, prs, err := statusPullRequests(ctx, s.forge, repo, group)
		if err != nil {
			return nil, err
		}
		for i, e := range group {
			pr := prs[i]
			if !opt.all && pr.State != "OPEN" {
				continue
			}
			row := statusRowDefaults(e, pr)
			if err := statusReview(repo, pr, viewer, &row); err != nil {
				return nil, err
			}
			ci, err := statusCI(repo, pr)
			if err != nil {
				return nil, err
			}
			row.CI = ci
			mainRed, promotable, err := r.statusHooks(ctx, s.forge, e, pr, &row)
			if err != nil {
				return nil, err
			}
			parentLanded, err := r.statusParentLanded(repo, e.Bookmark)
			if err != nil {
				return nil, err
			}
			row.Next = nextAction(row, parentLanded, mainRed, promotable)
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func statusRowDefaults(e LedgerEntry, pr statusPR) StatusRow {
	row := StatusRow{Repo: e.Repo, PR: e.PR, Bookmark: e.Bookmark, State: strings.ToLower(pr.State), Conflicts: "unknown", Hooks: "absent", Review: "none", Approval: "unknown"}
	if row.State == "open" {
		if pr.IsDraft {
			row.State = "draft"
		} else {
			row.State = "ready"
		}
	}
	switch pr.Mergeable {
	case "MERGEABLE":
		row.Conflicts = "clean"
	case "CONFLICTING":
		row.Conflicts = "conflicting"
	}
	return row
}

func (r *repository) statusParentLanded(repo, bookmark string) (bool, error) {
	path, err := (fileBaseline{root: r.stateDir, keys: r.keys}).path(bookmark, ".parent.json")
	if err != nil || repo != r.profile.Name {
		return false, err
	}
	data, err := os.ReadFile(path) //nolint:gosec // The bookmark path was validated above.
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var parent parentRelationship
	if err := json.Unmarshal(data, &parent); err != nil {
		return false, err
	}
	return parent.Landed, nil
}

func (r *repository) statusHooks(ctx context.Context, forge *forgeClient, e LedgerEntry, pr statusPR, row *StatusRow) (bool, bool, error) {
	rec, found, err := (localPrePush{root: r.stateDir}).Read(ctx, e.Repo, e.PR, pr.HeadRefOid)
	if err != nil || !found {
		return false, false, err
	}
	var blob string
	if len(pr.Commits.Nodes) > 0 {
		for _, entry := range pr.Commits.Nodes[0].Commit.Tree.Entries {
			if entry.Name == hkConfigName {
				blob = entry.Object.Oid
				break
			}
		}
	}
	var comparison struct {
		MergeBaseCommit struct {
			SHA string `json:"sha"`
		} `json:"merge_base_commit"`
	}
	path := "/repos/" + e.Repo + "/compare/" + url.PathEscape(pr.BaseRefName) + "..." + pr.HeadRefOid
	if err := forge.api(ctx, http.MethodGet, path, nil, &comparison); err != nil {
		return false, false, err
	}
	identity := HookRecord{Repo: e.Repo, Head: pr.HeadRefOid, Base: comparison.MergeBaseCommit.SHA, HookConfigBlob: blob}
	if !statusEvidenceCurrent(rec, identity) {
		row.Hooks = "stale"
		return false, false, nil
	}
	row.Hooks = string(rec.Result)
	// mainRed: eligible for ready --main-red. promotable: plain ready may promote it.
	mainRed := rec.Result == hookPartial || (rec.Result == hookSkipped && strings.HasPrefix(rec.Skip, "main-red:"))
	promotable := rec.Result == hookSkipped && !strings.HasPrefix(rec.Skip, "main-red:")
	return mainRed, promotable, nil
}

func statusCI(repo string, pr statusPR) (string, error) {
	red, pending := []string{}, []string{}
	for _, commit := range pr.Commits.Nodes {
		contexts := commit.Commit.StatusCheckRollup.Contexts
		if contexts.PageInfo.HasNextPage {
			return "", fmt.Errorf("status checks for %s exceed one page", repo)
		}
		for _, check := range contexts.Nodes {
			name, state := check.Name, strings.ToUpper(check.Conclusion)
			if check.Typename == "StatusContext" {
				name, state = check.Context, strings.ToUpper(check.State)
			} else if check.Status != "COMPLETED" {
				state = "PENDING"
			}
			switch state {
			case "SUCCESS", "NEUTRAL", "SKIPPED":
			case "PENDING", "EXPECTED", "":
				pending = append(pending, name)
			default:
				red = append(red, name)
			}
		}
	}
	sort.Strings(red)
	sort.Strings(pending)
	if len(red) > 0 {
		return "red: " + strings.Join(red, ", "), nil
	}
	if len(pending) > 0 {
		return "pending: " + strings.Join(pending, ", "), nil
	}
	return "green", nil
}

func statusReview(repo string, pr statusPR, viewer string, row *StatusRow) error {
	for _, c := range pr.Comments.Nodes {
		for _, m := range ParseMarkers(c.Body, c.Author.Login, c.Url) {
			if m.Kind == markerAllClear && m.Author == viewer {
				if m.Reviewed == pr.HeadRefOid && m.Base == pr.BaseRefName {
					row.Review = string(markerAllClear)
				} else if row.Review != string(markerAllClear) {
					row.Review = "stale"
				}
			}
		}
	}
	if pr.Comments.PageInfo.HasNextPage {
		return fmt.Errorf("status comments for %s exceed one page", repo)
	}
	if pr.TimelineItems.PageInfo.HasNextPage {
		return fmt.Errorf("status timeline for %s exceed one page", repo)
	}
	events := []TimelineEvent{}
	for _, event := range pr.TimelineItems.Nodes {
		e := TimelineEvent{Index: len(events), ReviewID: event.DatabaseId, State: event.State, DismissedReviewID: event.Review.DatabaseId}
		switch event.Typename {
		case "PullRequestReview":
			e.Event = "reviewed"
		case "ReviewDismissedEvent":
			e.Event = "review_dismissed"
		case "HeadRefForcePushedEvent":
			e.Event = "head_ref_force_pushed"
		case "PullRequestCommit":
			e.Event = "committed"
		}
		events = append(events, e)
	}
	switch ApprovalCoverage(events, !pr.TimelineItems.PageInfo.HasNextPage) {
	case Covered:
		row.Approval = "covered"
	case Uncovered:
		row.Approval = "uncovered"
	case Unknown:
	}
	return nil
}
