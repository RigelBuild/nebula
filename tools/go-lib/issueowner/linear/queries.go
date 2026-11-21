// SPDX-License-Identifier: MIT OR Apache-2.0

package linear

import (
	"context"
	"errors"
	"fmt"

	"github.com/RigelBuild/nebula/tools/go-lib/issueowner"
)

// graphQLRequest executes a query and decodes its data into the page response shape.
type graphQLRequest func(context.Context, string, map[string]any) (pageResponseData, error)

// pageResponseData is the decoded response shape used by the page query.
type pageResponseData struct {
	Issues struct {
		Nodes    []issueNode `json:"nodes"`
		PageInfo struct {
			HasNextPage bool    `json:"hasNextPage"`
			EndCursor   *string `json:"endCursor"`
		} `json:"pageInfo"`
	} `json:"issues"`
}

// issueNode is one page-query result before projection onto seam fields.
type issueNode struct {
	Identifier  string       `json:"identifier"`
	Title       string       `json:"title"`
	URL         string       `json:"url"`
	Description *string      `json:"description"`
	State       *issueState  `json:"state"`
	Parent      *issueParent `json:"parent"`
	CompletedAt *string      `json:"completedAt"`
}

// issueState is a nested state in a Linear issue response.
type issueState struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// issueParent is the parent identifier returned by Linear.
type issueParent struct {
	Identifier string `json:"identifier"`
}

const (
	linearGraphQLEndpoint = "https://api.linear.app/graphql"
	linearTeamKey         = "RIG"
	linearPageSize        = 100
)
const linearPageQuery = `
query TeamIssuesPage($teamKey: String!, $first: Int!, $after: String) {
 issues(filter: { team: { key: { eq: $teamKey } } }, first: $first, after: $after) {
  nodes { identifier title url description state { name type } parent { identifier } completedAt }
  pageInfo { hasNextPage endCursor }
 }
}`

// fetchPageWith makes one batched request and refuses a page that would truncate a scan.
func fetchPageWith(ctx context.Context, request graphQLRequest, after string) (issueowner.LinearPage, error) {
	var cursor any
	if after != "" {
		cursor = after
	}
	response, err := request(ctx, linearPageQuery, map[string]any{"teamKey": linearTeamKey, "first": linearPageSize, "after": cursor})
	if err != nil {
		return issueowner.LinearPage{}, fmt.Errorf("fetching Linear issue page: %w", err)
	}
	page := response.Issues
	if page.PageInfo.HasNextPage && (page.PageInfo.EndCursor == nil || *page.PageInfo.EndCursor == "") {
		return issueowner.LinearPage{}, errors.New("linear reported hasNextPage with no endCursor; refusing to report a truncated scan as complete")
	}
	issues := make([]issueowner.LinearIssueFields, 0, len(page.Nodes))
	for _, node := range page.Nodes {
		fields := issueowner.LinearIssueFields{Identifier: node.Identifier, Title: node.Title, URL: node.URL, Description: node.Description, CompletedAt: node.CompletedAt}
		if node.State != nil {
			fields.StateName, fields.StateType = node.State.Name, node.State.Type
		}
		if node.Parent != nil {
			fields.ParentIdentifier = node.Parent.Identifier
		}
		issues = append(issues, fields)
	}
	result := issueowner.LinearPage{Issues: issues}
	if page.PageInfo.EndCursor != nil {
		result.EndCursor = *page.PageInfo.EndCursor
	}
	return result, nil
}

// FetchIssue fetches one issue by identifier.
func (c *IssueClient) FetchIssue(ctx context.Context, identifier string) (issueowner.LinearIssueFields, error) {
	const query = `query Issue($id: String!) { issue(id: $id) { identifier title url description state { name type } parent { identifier } completedAt } }`
	var response struct {
		Issue *issueNode `json:"issue"`
	}
	if err := c.request(ctx, query, map[string]any{"id": identifier}, &response); err != nil {
		return issueowner.LinearIssueFields{}, fmt.Errorf("fetching Linear issue %s: %w", identifier, err)
	}
	if response.Issue == nil {
		return issueowner.LinearIssueFields{}, fmt.Errorf("fetching Linear issue %s: response omitted issue", identifier)
	}
	return fieldsFromNode(*response.Issue), nil
}

func fieldsFromNode(node issueNode) issueowner.LinearIssueFields {
	fields := issueowner.LinearIssueFields{Identifier: node.Identifier, Title: node.Title, URL: node.URL, Description: node.Description, CompletedAt: node.CompletedAt}
	if node.State != nil {
		fields.StateName, fields.StateType = node.State.Name, node.State.Type
	}
	if node.Parent != nil {
		fields.ParentIdentifier = node.Parent.Identifier
	}
	return fields
}

// FetchPage fetches one all-state page for the RIG team.
func (c *IssueClient) FetchPage(ctx context.Context, after string) (issueowner.LinearPage, error) {
	return fetchPageWith(ctx, func(ctx context.Context, query string, variables map[string]any) (pageResponseData, error) {
		var response pageResponseData
		if err := c.request(ctx, query, variables, &response); err != nil {
			return pageResponseData{}, err
		}
		return response, nil
	}, after)
}
