package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"sort"
	"strconv"
	"time"
)

const (
	linearGraphQLURL = "https://api.linear.app/graphql"
	chunkSize        = 50
	deadlineMessage  = "Sweep deadline reached"
)

const commentsQueryTemplate = `query WatchedComments($numbers: [Float!]!) {
  viewer { id }
  issues(first: 50, filter: { team: { key: { in: [{{TEAM_KEYS}}] } }, number: { in: $numbers } }) {
    nodes {
      identifier
      state { name }
      assignee { name }
      comments(first: 20, orderBy: createdAt) {
        nodes {
          id createdAt editedAt body
          user { id name } botActor { name } externalUser { name }
        }
        pageInfo { hasNextPage }
      }
    }
    pageInfo { hasNextPage }
  }
}`

// The watched team keys are configured per run; see prefixes.go.

type chunkResult struct {
	ViewerID           string
	Issues             orderedMap[Issue]
	RateLimitRemaining *float64
	Complexity         *float64
}

func parseChunkResponse(prefixes issuePrefixes, status int, body json.RawMessage, rateLimitRemaining *float64) (string, orderedMap[Issue], error) {
	if status != http.StatusOK {
		var payload struct {
			Errors []struct {
				Extensions struct {
					Code string `json:"code"`
				} `json:"extensions"`
			} `json:"errors"`
		}
		if json.Unmarshal(body, &payload) == nil {
			for _, graphqlError := range payload.Errors {
				if graphqlError.Extensions.Code == "RATELIMITED" {
					return "", orderedMap[Issue]{}, &SweepError{Message: "Linear rate limit exceeded", Kind: ErrorRateLimited, RateLimitRemaining: rateLimitRemaining}
				}
			}
		}
		return "", orderedMap[Issue]{}, &SweepError{Message: fmt.Sprintf("Linear GraphQL returned HTTP %d", status), Kind: ErrorHTTP, RateLimitRemaining: rateLimitRemaining}
	}
	var payload struct {
		Errors []json.RawMessage `json:"errors"`
		Data   struct {
			Viewer *struct {
				ID string `json:"id"`
			} `json:"viewer"`
			Issues *struct {
				Nodes    []json.RawMessage `json:"nodes"`
				PageInfo json.RawMessage   `json:"pageInfo"`
			} `json:"issues"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || len(payload.Errors) > 0 {
		return "", orderedMap[Issue]{}, &SweepError{Message: "Linear GraphQL returned errors", Kind: ErrorGraphQL, RateLimitRemaining: rateLimitRemaining}
	}
	if payload.Data.Viewer == nil || payload.Data.Viewer.ID == "" {
		return "", orderedMap[Issue]{}, &SweepError{Message: "Linear response viewer is malformed", Kind: ErrorBadShape, RateLimitRemaining: rateLimitRemaining}
	}
	if payload.Data.Issues == nil {
		return "", orderedMap[Issue]{}, &SweepError{Message: "Linear response issues are malformed", Kind: ErrorBadShape, RateLimitRemaining: rateLimitRemaining}
	}
	more, err := parseHasNextPage(payload.Data.Issues.PageInfo, "issues")
	if err != nil {
		return "", orderedMap[Issue]{}, withRateLimit(err, rateLimitRemaining)
	}
	if more {
		return "", orderedMap[Issue]{}, &SweepError{Message: "Linear returned more than one issue page", Kind: ErrorBadShape, RateLimitRemaining: rateLimitRemaining}
	}
	issues := newOrderedMap[Issue]()
	for index, rawIssue := range payload.Data.Issues.Nodes {
		issue, identifier, err := parseIssue(prefixes, rawIssue, index+1)
		if err != nil {
			return "", orderedMap[Issue]{}, withRateLimit(err, rateLimitRemaining)
		}
		if _, exists := issues.get(identifier); exists {
			return "", orderedMap[Issue]{}, &SweepError{Message: fmt.Sprintf("issue %d has an invalid or duplicate identifier", index+1), Kind: ErrorBadShape, RateLimitRemaining: rateLimitRemaining}
		}
		issues.set(identifier, issue)
	}
	return payload.Data.Viewer.ID, issues, nil
}

func withRateLimit(err error, remaining *float64) error {
	if sweepError, ok := errors.AsType[*SweepError](err); ok {
		sweepError.RateLimitRemaining = remaining
		return sweepError
	}
	return err
}

func parseHasNextPage(raw json.RawMessage, location string) (bool, error) {
	var value struct {
		HasNextPage *bool `json:"hasNextPage"`
	}
	if json.Unmarshal(raw, &value) != nil || value.HasNextPage == nil {
		return false, &SweepError{Message: location + " pageInfo is malformed", Kind: ErrorBadShape}
	}
	return *value.HasNextPage, nil
}

type optionalName struct {
	value *string
}

func parseNameObject(raw json.RawMessage, location string) (optionalName, error) {
	if bytes.Equal(raw, []byte("null")) {
		return optionalName{}, nil
	}
	var value struct {
		Name *string `json:"name"`
	}
	if json.Unmarshal(raw, &value) != nil || value.Name == nil {
		return optionalName{}, &SweepError{Message: location + " is malformed", Kind: ErrorBadShape}
	}
	return optionalName{value: value.Name}, nil
}

func parseIssue(prefixes issuePrefixes, raw json.RawMessage, index int) (Issue, string, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return Issue{}, "", badShape(fmt.Sprintf("issue %d is malformed", index))
	}
	var identifier string
	if json.Unmarshal(fields["identifier"], &identifier) != nil || identifier == "" {
		return Issue{}, "", badShape(fmt.Sprintf("issue %d is malformed", index))
	}
	canonical, ok := prefixes.normaliseIssueID(identifier)
	if !ok {
		return Issue{}, "", badShape(fmt.Sprintf("issue %d has an invalid or duplicate identifier", index))
	}
	identifier = canonical
	var state struct {
		Name *string `json:"name"`
	}
	if json.Unmarshal(fields["state"], &state) != nil || state.Name == nil {
		return Issue{}, "", badShape("issue " + identifier + " state is malformed")
	}
	parsedAssignee, err := parseNameObject(fields["assignee"], "issue "+identifier+" assignee")
	if err != nil {
		return Issue{}, "", err
	}
	assignee := parsedAssignee.value
	var connection struct {
		Nodes    []json.RawMessage `json:"nodes"`
		PageInfo json.RawMessage   `json:"pageInfo"`
	}
	if json.Unmarshal(fields["comments"], &connection) != nil || connection.Nodes == nil {
		return Issue{}, "", badShape("issue " + identifier + " comments are malformed")
	}
	hasMore, err := parseHasNextPage(connection.PageInfo, "issue "+identifier+" comments")
	if err != nil {
		return Issue{}, "", err
	}
	type timestampedComment struct {
		comment Comment
		time    time.Time
	}
	comments := make([]timestampedComment, 0, len(connection.Nodes))
	for commentIndex, rawComment := range connection.Nodes {
		comment, timestamp, err := parseLinearComment(rawComment, commentIndex+1)
		if err != nil {
			return Issue{}, "", withIssuePrefix(err, identifier)
		}
		comments = append(comments, timestampedComment{comment: comment, time: timestamp})
	}
	if hasMore && len(comments) == 0 {
		return Issue{}, "", badShape("issue " + identifier + " has an empty truncated comment page")
	}
	newestFirst := len(comments) < 2 || !comments[0].time.Before(comments[len(comments)-1].time)
	sort.SliceStable(comments, func(left, right int) bool { return comments[left].time.Before(comments[right].time) })
	sortedComments := make([]Comment, 0, len(comments))
	for _, comment := range comments {
		sortedComments = append(sortedComments, comment.comment)
	}
	truncated := TruncatedNone
	if hasMore && newestFirst {
		truncated = TruncatedOlder
	} else if hasMore {
		truncated = TruncatedNewer
	}
	return Issue{State: *state.Name, Assignee: assignee, Truncated: truncated, Comments: sortedComments}, identifier, nil
}

func withIssuePrefix(err error, identifier string) error {
	if sweepError, ok := errors.AsType[*SweepError](err); ok {
		sweepError.Message = "issue " + identifier + " " + sweepError.Message
	}
	return err
}

func parseLinearComment(raw json.RawMessage, index int) (Comment, time.Time, error) {
	var fields map[string]json.RawMessage
	var comment Comment
	if json.Unmarshal(raw, &fields) != nil || fields == nil ||
		json.Unmarshal(raw, &comment) != nil || comment.ID == "" || !validDate(comment.CreatedAt) ||
		!validNullableDate(fields["editedAt"]) {
		return Comment{}, time.Time{}, badShape(fmt.Sprintf("comment %d is malformed", index))
	}
	if _, ok := fields["body"]; !ok {
		return Comment{}, time.Time{}, badShape(fmt.Sprintf("comment %d is malformed", index))
	}
	userRaw := fields["user"]
	if len(userRaw) == 0 {
		return Comment{}, time.Time{}, badShape(fmt.Sprintf("comment %d user is malformed", index))
	}
	var userID, userName *string
	if !bytes.Equal(userRaw, []byte("null")) {
		var user struct {
			ID   *string `json:"id"`
			Name *string `json:"name"`
		}
		if json.Unmarshal(userRaw, &user) != nil || user.ID == nil || user.Name == nil {
			return Comment{}, time.Time{}, badShape(fmt.Sprintf("comment %d user is malformed", index))
		}
		userID, userName = user.ID, user.Name
	}
	parsedBotName, err := parseNameObject(fields["botActor"], fmt.Sprintf("comment %d botActor", index))
	if err != nil {
		return Comment{}, time.Time{}, err
	}
	parsedExternalName, err := parseNameObject(fields["externalUser"], fmt.Sprintf("comment %d externalUser", index))
	if err != nil {
		return Comment{}, time.Time{}, err
	}
	botName, externalName := parsedBotName.value, parsedExternalName.value
	switch {
	case userName != nil && *userName != "":
		comment.Author = *userName
		if botName != nil && *botName != "" {
			comment.Author += " via " + *botName
		}
	case botName != nil:
		comment.Author = *botName
	case externalName != nil:
		comment.Author = *externalName
	default:
		comment.Author = "unknown"
	}
	comment.AuthorID = userID
	timestamp, _ := parseTime(comment.CreatedAt)
	return comment, timestamp, nil
}

func badShape(message string) *SweepError {
	return &SweepError{Message: message, Kind: ErrorBadShape}
}

func fetchChunk(ctx context.Context, prefixes issuePrefixes, numbers []int64, accessToken string, client *http.Client, timeout time.Duration) (chunkResult, error) {
	if timeout <= 0 {
		return chunkResult{}, &SweepError{Message: deadlineMessage, Kind: ErrorDeadline}
	}
	requestContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	encoded, err := json.Marshal(struct {
		Query     string `json:"query"`
		Variables struct {
			Numbers []int64 `json:"numbers"`
		} `json:"variables"`
	}{Query: commentsQuery(prefixes), Variables: struct {
		Numbers []int64 `json:"numbers"`
	}{Numbers: numbers}})
	if err != nil {
		return chunkResult{}, fmt.Errorf("encoding Linear GraphQL request: %w", err)
	}
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, linearGraphQLURL, bytes.NewReader(encoded))
	if err != nil {
		return chunkResult{}, fmt.Errorf("creating Linear GraphQL request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+accessToken)
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		if requestContext.Err() != nil || isTimeoutError(err) {
			return chunkResult{}, &SweepError{Message: "Linear request timed out", Kind: ErrorTimeout}
		}
		return chunkResult{}, &SweepError{Message: "Linear request failed: network error (" + errorCause(err) + ")", Kind: ErrorNetwork}
	}
	remaining := headerFloat(response.Header.Get("X-Ratelimit-Requests-Remaining"))
	complexity := headerFloat(response.Header.Get("X-Complexity"))
	body, readErr := io.ReadAll(response.Body)
	readErr = errors.Join(readErr, response.Body.Close())
	if readErr != nil {
		if requestContext.Err() != nil {
			return chunkResult{}, &SweepError{Message: "Linear request timed out", Kind: ErrorTimeout, RateLimitRemaining: remaining}
		}
		return chunkResult{}, &SweepError{Message: "Linear request failed: network error (" + errorCause(readErr) + ")", Kind: ErrorNetwork, RateLimitRemaining: remaining}
	}
	if !json.Valid(body) && response.StatusCode == http.StatusOK {
		return chunkResult{}, &SweepError{Message: "Linear response body is not valid JSON", Kind: ErrorBadShape, RateLimitRemaining: remaining}
	}
	viewerID, issues, err := parseChunkResponse(prefixes, response.StatusCode, body, remaining)
	if err != nil {
		return chunkResult{}, err
	}
	return chunkResult{ViewerID: viewerID, Issues: issues, RateLimitRemaining: remaining, Complexity: complexity}, nil
}

func headerFloat(value string) *float64 {
	if value == "" {
		return nil
	}
	number, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
		return nil
	}
	return new(number)
}

func isTimeoutError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}
