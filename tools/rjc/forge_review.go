package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

const graphqlQueryKey = "query"
const graphqlVariablesKey = "variables"

func (f *forgeClient) viewer(ctx context.Context) (string, error) {
	var user struct{ Login string }
	if err := f.api(ctx, http.MethodGet, "/user", nil, &user); err != nil {
		return "", err
	}
	if user.Login == "" {
		return "", errors.New("forge returned an empty viewer login")
	}
	return user.Login, nil
}

type issueComment struct {
	Body    string
	HTMLURL string `json:"html_url"`
	User    struct{ Login string }
}

func (f *forgeClient) issueComments(ctx context.Context, repo string, pr int) ([]issueComment, error) {
	all := []issueComment{}
	for page := 1; ; page++ {
		var comments []issueComment
		path := fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100&page=%d", repo, pr, page)
		if err := f.api(ctx, http.MethodGet, path, nil, &comments); err != nil {
			return nil, err
		}
		all = append(all, comments...)
		if len(comments) < 100 {
			return all, nil
		}
	}
}

func (f *forgeClient) comments(ctx context.Context, repo string, pr int) ([]Marker, error) {
	comments, err := f.issueComments(ctx, repo, pr)
	if err != nil {
		return nil, err
	}
	markers := []Marker{}
	for _, comment := range comments {
		markers = append(markers, ParseMarkers(comment.Body, comment.User.Login, comment.HTMLURL)...)
	}
	return markers, nil
}

func (f *forgeClient) postComment(ctx context.Context, repo string, pr int, body string) (string, error) {
	var result struct {
		HTMLURL string `json:"html_url"`
	}
	request := struct {
		Body string `json:"body"`
	}{Body: body}
	err := f.api(ctx, http.MethodPost, "/repos/"+repo+"/issues/"+strconv.Itoa(pr)+"/comments", request, &result)
	return result.HTMLURL, err
}

func (f *forgeClient) markReady(ctx context.Context, repo string, pr int) error {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return fmt.Errorf("invalid repository %q", repo)
	}
	var lookup struct {
		Data struct {
			Repository struct{ PullRequest struct{ ID string } }
		}
		Errors []struct{ Message string }
	}
	query := `query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){id}}}`
	if err := f.api(ctx, http.MethodPost, "/graphql", map[string]any{graphqlQueryKey: query, graphqlVariablesKey: map[string]any{"owner": owner, "name": name, "number": pr}}, &lookup); err != nil {
		return err
	}
	if len(lookup.Errors) > 0 || lookup.Data.Repository.PullRequest.ID == "" {
		return errors.New("cannot resolve pull request node for promotion")
	}
	var result struct {
		Data struct {
			MarkPullRequestReadyForReview struct{ PullRequest struct{ IsDraft bool } }
		}
		Errors []struct{ Message string }
	}
	mutation := `mutation($id:ID!){markPullRequestReadyForReview(input:{pullRequestId:$id}){pullRequest{isDraft}}}`
	if err := f.api(ctx, http.MethodPost, "/graphql", map[string]any{graphqlQueryKey: mutation, graphqlVariablesKey: map[string]any{"id": lookup.Data.Repository.PullRequest.ID}}, &result); err != nil {
		return err
	}
	if len(result.Errors) > 0 {
		return fmt.Errorf("forge rejected ready mutation: %s", result.Errors[0].Message)
	}
	readback, err := f.metadata(ctx, repo, pr)
	if err != nil {
		return err
	}
	if readback.Draft {
		return errors.New("pull request remains draft after promotion")
	}
	return nil
}
