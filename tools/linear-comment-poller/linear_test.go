package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func linearComment(id, createdAt, name string) map[string]any {
	return map[string]any{"id": id, "createdAt": createdAt, "editedAt": nil, "body": id + " body", "user": map[string]any{"id": "user-" + id, "name": name}, "botActor": nil, "externalUser": nil}
}

func graphBody(comments []any, hasMore bool) map[string]any {
	if comments == nil {
		comments = []any{}
	}
	return map[string]any{"data": map[string]any{
		"viewer": map[string]any{"id": "app-viewer"},
		"issues": map[string]any{
			"nodes":    []any{map[string]any{"identifier": "EX-12", "state": map[string]any{"name": "Todo"}, "assignee": map[string]any{"name": "Worker"}, "comments": map[string]any{"nodes": comments, "pageInfo": map[string]any{"hasNextPage": hasMore}}}},
			"pageInfo": map[string]any{"hasNextPage": false},
		},
	}}
}

func TestGraphQLQueryAndParseChunkResponse(t *testing.T) {
	t.Run("uses fixed query bounds", func(t *testing.T) {
		if chunkSize != 50 || !strings.Contains(commentsQuery(examplePrefixes(t)), "viewer { id }") || !strings.Contains(commentsQuery(examplePrefixes(t)), "number: { in: $numbers }") || !strings.Contains(commentsQuery(examplePrefixes(t)), "comments(first: 20") || strings.Contains(commentsQuery(examplePrefixes(t)), "assignee: {") {
			t.Fatal("query contract changed")
		}
	})
	t.Run("sorts newest first and marks older comments missing", func(t *testing.T) {
		body, err := json.Marshal(graphBody([]any{linearComment("new", "2026-10-03T22:00:00.000Z", "New"), linearComment("old", "2026-10-03T21:00:00.000Z", "Old")}, true))
		if err != nil {
			t.Fatal(err)
		}
		_, issues, err := parseChunkResponse(examplePrefixes(t), 200, body, new(float64(4999)))
		issue, ok := issues.get("EX-12")
		if err != nil || !ok || issue.Truncated != TruncatedOlder || issue.Comments[0].ID != "old" || issue.Comments[1].ID != "new" {
			t.Fatalf("parsed = %#v, %v", issue, err)
		}
	})
	t.Run("sorts oldest first and marks newer comments missing", func(t *testing.T) {
		body, err := json.Marshal(graphBody([]any{linearComment("old", "2026-10-03T21:00:00.000Z", "Old"), linearComment("new", "2026-10-03T22:00:00.000Z", "New")}, true))
		if err != nil {
			t.Fatal(err)
		}
		_, issues, err := parseChunkResponse(examplePrefixes(t), 200, body, nil)
		issue, _ := issues.get("EX-12")
		if err != nil || issue.Truncated != TruncatedNewer || issue.Comments[0].ID != "old" {
			t.Fatalf("parsed = %#v, %v", issue, err)
		}
	})
	t.Run("formats bot authors and omits unknown issue IDs", func(t *testing.T) {
		comment := linearComment("bot", "2026-10-03T22:00:00.000Z", "Bot")
		comment["botActor"] = map[string]any{"name": "Linear"}
		body, err := json.Marshal(graphBody([]any{comment}, false))
		if err != nil {
			t.Fatal(err)
		}
		_, issues, err := parseChunkResponse(examplePrefixes(t), 200, body, nil)
		issue, _ := issues.get("EX-12")
		if err != nil || issue.Comments[0].Author != "Bot via Linear" {
			t.Fatalf("parsed = %#v, %v", issue, err)
		}
		unknown := map[string]any{"data": map[string]any{"viewer": map[string]any{"id": "app"}, "issues": map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}}}}
		body, err = json.Marshal(unknown)
		if err != nil {
			t.Fatal(err)
		}
		_, issues, err = parseChunkResponse(examplePrefixes(t), 200, body, nil)
		if err != nil || len(issues.keys) != 0 {
			t.Fatalf("empty response = %#v, %v", issues, err)
		}
	})
	for _, test := range []struct {
		name     string
		status   int
		body     any
		wantKind ErrorKind
		wantText string
	}{
		{name: "rate limited", status: 400, body: map[string]any{"errors": []any{map[string]any{"extensions": map[string]any{"code": "RATELIMITED"}}}}, wantKind: ErrorRateLimited},
		{name: "graphql errors", status: 200, body: map[string]any{"errors": []any{map[string]any{"message": "bad"}}}, wantKind: ErrorGraphQL, wantText: "GraphQL returned errors"},
		{name: "more issues page", status: 200, body: map[string]any{"data": map[string]any{"viewer": map[string]any{"id": "app"}, "issues": map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": true}}}}, wantKind: ErrorBadShape, wantText: "more than one issue page"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, _ := json.Marshal(test.body)
			_, _, err := parseChunkResponse(examplePrefixes(t), test.status, body, new(float64(0)))
			var sweepError *SweepError
			if !errors.As(err, &sweepError) || sweepError.Kind != test.wantKind || (test.wantText != "" && !strings.Contains(err.Error(), test.wantText)) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestIssueParseErrorsIncludeIssuePrefix(t *testing.T) {
	tests := []struct {
		name  string
		issue map[string]any
		want  string
	}{
		{name: "state", issue: map[string]any{"state": nil}, want: "issue EX-1 state is malformed"},
		{name: "assignee", issue: map[string]any{"assignee": map[string]any{}}, want: "issue EX-1 assignee is malformed"},
		{name: "comments", issue: map[string]any{"comments": nil}, want: "issue EX-1 comments are malformed"},
		{name: "comment page info", issue: map[string]any{"comments": map[string]any{"nodes": []any{}, "pageInfo": nil}}, want: "issue EX-1 comments pageInfo is malformed"},
		{name: "comment body", issue: map[string]any{"comments": map[string]any{"nodes": []any{map[string]any{}}, "pageInfo": map[string]any{"hasNextPage": false}}}, want: "issue EX-1 comment 1 is malformed"},
		{name: "empty truncated comment page", issue: map[string]any{"comments": map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": true}}}, want: "issue EX-1 has an empty truncated comment page"},
		{name: "state page info", issue: map[string]any{"state": "invalid"}, want: "issue EX-1 state is malformed"},
		{name: "missing state", issue: map[string]any{"state": nil}, want: "issue EX-1 state is malformed"},
		{name: "missing state name", issue: map[string]any{"state": map[string]any{}}, want: "issue EX-1 state is malformed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			issue := map[string]any{
				"identifier": "EX-1",
				"state":      map[string]any{"name": "Todo"},
				"assignee":   nil,
				"comments":   map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}},
			}
			maps.Copy(issue, test.issue)
			body, err := json.Marshal(map[string]any{"data": map[string]any{
				"viewer": map[string]any{"id": "app"},
				"issues": map[string]any{"nodes": []any{issue}, "pageInfo": map[string]any{"hasNextPage": false}},
			}})
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = parseChunkResponse(examplePrefixes(t), http.StatusOK, body, nil)
			if err == nil || err.Error() != test.want {
				t.Fatalf("parse error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestTimeoutDetectionUsesErrorType(t *testing.T) {
	if !isTimeoutError(context.DeadlineExceeded) || isTimeoutError(errors.New("Gateway Timeout")) {
		t.Fatal("timeout detection did not distinguish deadline errors from HTTP text")
	}
}

func TestFetchChunk(t *testing.T) {
	t.Run("uses bearer authorization and reads response headers", func(t *testing.T) {
		var request *http.Request
		var requestBody []byte
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
			request = incoming.Clone(incoming.Context())
			requestBody, _ = io.ReadAll(incoming.Body)
			writer.Header().Set("X-Ratelimit-Requests-Remaining", "4987")
			writer.Header().Set("X-Complexity", "13")
			body, err := json.Marshal(graphBody(nil, false))
			if err != nil {
				t.Errorf("marshal response: %v", err)
				return
			}
			if _, err := writer.Write(body); err != nil {
				t.Errorf("write response: %v", err)
			}
		}))
		defer server.Close()
		client := &http.Client{Transport: rewriteLinearHost{base: server.Client().Transport, host: strings.TrimPrefix(server.URL, "http://")}}
		result, err := fetchChunk(t.Context(), examplePrefixes(t), []int64{12}, "token", client, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if request == nil || request.Method != http.MethodPost || request.Header.Get("Authorization") != "Bearer token" || request.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("request = %#v", request)
		}
		var body struct {
			Variables struct {
				Numbers []int64 `json:"numbers"`
			} `json:"variables"`
		}
		if err := json.Unmarshal(requestBody, &body); err != nil || len(body.Variables.Numbers) != 1 || body.Variables.Numbers[0] != 12 {
			t.Fatalf("request body = %#v, %v", body, err)
		}
		if result.RateLimitRemaining == nil || *result.RateLimitRemaining != 4987 || result.Complexity == nil || *result.Complexity != 13 {
			t.Fatalf("response headers = %#v", result)
		}
	})
	t.Run("classifies network failures and timeout errors", func(t *testing.T) {
		for _, test := range []struct {
			name string
			err  error
			kind ErrorKind
		}{
			{name: "network", err: errors.New("connection refused"), kind: ErrorNetwork},
			{name: "timeout", err: context.DeadlineExceeded, kind: ErrorTimeout},
		} {
			t.Run(test.name, func(t *testing.T) {
				client := &http.Client{Transport: roundTripError{err: test.err}}
				_, err := fetchChunk(t.Context(), examplePrefixes(t), []int64{12}, "token", client, time.Second)
				var sweepError *SweepError
				if !errors.As(err, &sweepError) || sweepError.Kind != test.kind {
					t.Fatalf("fetch error = %v, want kind %s", err, test.kind)
				}
			})
		}
	})
	t.Run("preserves explicit timeout error as timeout", func(t *testing.T) {
		client := &http.Client{Transport: roundTripError{err: context.DeadlineExceeded}}
		_, err := fetchChunk(t.Context(), examplePrefixes(t), []int64{12}, "token", client, time.Second)
		var sweepError *SweepError
		if !errors.As(err, &sweepError) || sweepError.Kind != ErrorTimeout {
			t.Fatalf("fetch error = %v, want timeout", err)
		}
	})
	t.Run("times out after response headers when body stalls", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusOK)
			writer.(http.Flusher).Flush()
			<-request.Context().Done()
		}))
		defer server.Close()
		client := &http.Client{Transport: rewriteLinearHost{base: server.Client().Transport, host: strings.TrimPrefix(server.URL, "http://")}}
		_, err := fetchChunk(t.Context(), examplePrefixes(t), []int64{12}, "token", client, time.Millisecond)
		var sweepError *SweepError
		if !errors.As(err, &sweepError) || sweepError.Kind != ErrorTimeout {
			t.Fatalf("fetch error = %v", err)
		}
	})
	t.Run("preserves cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := fetchChunk(ctx, examplePrefixes(t), []int64{12}, "token", http.DefaultClient, time.Second)
		if err == nil {
			t.Fatal("cancelled request succeeded")
		}
	})
}

type roundTripError struct {
	err error
}

func (transport roundTripError) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, transport.err
}

type rewriteLinearHost struct {
	base http.RoundTripper
	host string
}

func (transport rewriteLinearHost) RoundTrip(request *http.Request) (*http.Response, error) {
	copy := request.Clone(request.Context())
	copy.URL.Scheme = "http"
	copy.URL.Host = transport.host
	copy.Host = transport.host
	return transport.base.RoundTrip(copy)
}

func TestFetchHeadersParse(t *testing.T) {
	for _, test := range []struct {
		value string
		want  *float64
	}{
		{value: "", want: nil},
		{value: "not-number", want: nil},
		{value: "NaN", want: nil},
		{value: "4987", want: new(float64(4987))},
	} {
		t.Run(strconv.Quote(test.value), func(t *testing.T) {
			got := headerFloat(test.value)
			if got == nil && test.want == nil {
				return
			}
			if got == nil || test.want == nil || *got != *test.want {
				t.Fatalf("headerFloat(%q) = %v, want %v", test.value, got, test.want)
			}
		})
	}
}
