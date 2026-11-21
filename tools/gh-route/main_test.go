package main

import (
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

func mustEncodeRateLimit(snapshot RateLimit) string {
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}
func intValue(value int64) *int64 { return new(value) }

func testRateLimit(rest, restLimit, graphql, graphqlLimit, reset int64) RateLimit {
	return RateLimit{Resources: &Resources{
		Core:    &Bucket{Remaining: intValue(rest), Limit: intValue(restLimit), Reset: intValue(reset)},
		GraphQL: &Bucket{Remaining: intValue(graphql), Limit: intValue(graphqlLimit), Reset: intValue(reset)},
	}}
}

func decodedJSON(t *testing.T, text string) any {
	t.Helper()
	var result any
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func encodeJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestPick(t *testing.T) {
	const floor = 200
	for _, test := range []struct {
		name      string
		rateLimit RateLimit
		want      string
	}{
		{"graphql-dominant fraction → graphql", testRateLimit(100, 5000, 4000, 5000, 9999), "graphql"},
		{"rest-dominant fraction → rest", testRateLimit(4000, 5000, 100, 5000, 9999), "rest"},
		{"equal fraction → rest (tie to idle bucket)", testRateLimit(2500, 5000, 2500, 5000, 9999), "rest"},
		{"graphql exhausted → rest", testRateLimit(500, 5000, 0, 5000, 9999), "rest"},
		{"graphql at/below floor but higher fraction → rest (guard overrides)", testRateLimit(300, 5000, 199, 1000, 9999), "rest"},
		{"rest at/below floor but higher fraction → graphql (guard overrides)", testRateLimit(199, 1000, 300, 5000, 9999), "graphql"},
		{"graphql exactly at floor → rest (≤ boundary is guarded)", testRateLimit(300, 5000, 200, 1000, 9999), "rest"},
		{"graphql one above floor → graphql (healthy, wins on fraction)", testRateLimit(300, 5000, 201, 1000, 9999), "graphql"},
		{"both healthy, graphql higher fraction → graphql", testRateLimit(3000, 5000, 4000, 5000, 9999), "graphql"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := pick(test.rateLimit, floor); got != test.want {
				t.Fatalf("pick() = %q, want %q", got, test.want)
			}
		})
	}
	t.Run("zero-limit bucket scores 0 fraction", func(t *testing.T) {
		if got := fraction(&Bucket{Remaining: intValue(100), Limit: intValue(0)}); got != 0 {
			t.Fatalf("zero limit score = %f", got)
		}
		if got := fraction(nil); got != 0 {
			t.Fatalf("missing bucket score = %f", got)
		}
		if got := fraction(&Bucket{Remaining: intValue(2500), Limit: intValue(5000)}); got != 500 {
			t.Fatalf("fraction = %f", got)
		}
	})
}

func TestRemaining(t *testing.T) {
	snapshot := testRateLimit(4321, 5000, 1234, 5000, 9999)
	for _, test := range []struct {
		name  string
		which string
		want  int64
	}{
		{"remaining rest → core count", "rest", 4321},
		{"remaining graphql → graphql count", "graphql", 1234},
		{"any non-graphql arg maps to core", "", 4321},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := remaining(snapshot, test.which); got != test.want {
				t.Fatalf("remaining() = %d, want %d", got, test.want)
			}
		})
	}
	t.Run("missing bucket → 0", func(t *testing.T) {
		for _, bucket := range []string{"rest", "graphql"} {
			if got := remaining(RateLimit{}, bucket); got != 0 {
				t.Errorf("remaining(%q) without resources = %d", bucket, got)
			}
		}
	})
}

const (
	reviewsRESTJSON           = `[{"user":{"login":"alice"},"state":"APPROVED","submitted_at":"2024-01-01T00:00:00Z","commit_id":"aaa111","body":"lgtm"},{"user":{"login":"seal-bot[bot]"},"state":"COMMENTED","submitted_at":"2024-01-02T00:00:00Z","commit_id":"bbb222","body":"nit: rename"}]`
	reviewsGraphQLJSON        = `{"data":{"repository":{"pullRequest":{"reviews":{"nodes":[{"author":{"login":"alice","__typename":"User"},"state":"APPROVED","submittedAt":"2024-01-01T00:00:00Z","commit":{"oid":"aaa111"},"body":"lgtm"},{"author":{"login":"seal-bot","__typename":"Bot"},"state":"COMMENTED","submittedAt":"2024-01-02T00:00:00Z","commit":{"oid":"bbb222"},"body":"nit: rename"}]}}}}}`
	commentsRESTJSON          = `[{"user":{"login":"alice"},"body":"hi","created_at":"2024-01-01T00:00:00Z","updated_at":"2024-01-01T01:00:00Z"},{"user":{"login":"seal-bot[bot]"},"body":"CI passed","created_at":"2024-01-02T00:00:00Z","updated_at":"2024-01-02T00:00:00Z"}]`
	commentsGraphQLJSON       = `{"data":{"repository":{"pullRequest":{"comments":{"nodes":[{"author":{"login":"alice","__typename":"User"},"body":"hi","createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T01:00:00Z"},{"author":{"login":"seal-bot","__typename":"Bot"},"body":"CI passed","createdAt":"2024-01-02T00:00:00Z","updatedAt":"2024-01-02T00:00:00Z"}]}}}}}`
	reviewCommentsRESTJSON    = `[{"user":{"login":"alice"},"body":"style","path":"src/a.ts","line":10,"commit_id":"ccc333"},{"user":{"login":"seal-bot[bot]"},"body":"unused var","path":"src/b.ts","line":20,"commit_id":"ddd444"}]`
	reviewCommentsGraphQLJSON = `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"comments":{"nodes":[{"author":{"login":"alice","__typename":"User"},"body":"style","path":"src/a.ts","line":10,"commit":{"oid":"ccc333"},"originalCommit":{"oid":"zzz000"}},{"author":{"login":"seal-bot","__typename":"Bot"},"body":"unused var","path":"src/b.ts","line":20,"commit":{"oid":"ddd444"},"originalCommit":{"oid":"zzz000"}}]}}]}}}}}`
	headRESTJSON              = `{"head":{"sha":"c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00"}}`
	headGraphQLJSON           = `{"data":{"repository":{"pullRequest":{"headRefOid":"c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00"}}}}`
	checkRunsRESTJSON         = `{"check_runs":[{"name":"build","status":"completed","conclusion":"success"},{"name":"lint","status":"queued","conclusion":null}]}`
	checkRunsGraphQLJSON      = `{"data":{"repository":{"object":{"checkSuites":{"nodes":[{"checkRuns":{"nodes":[{"name":"build","status":"COMPLETED","conclusion":"SUCCESS"}]}},{"checkRuns":{"nodes":[{"name":"lint","status":"QUEUED","conclusion":null}]}}]}}}}}`
	pullRequestsRESTJSON      = `[{"number":7,"title":"Add feature","state":"open","head":{"sha":"abc123"},"user":{"login":"alice"}},{"number":8,"title":"Fix bug","state":"open","head":{"sha":"def456"},"user":{"login":"dependabot[bot]"}}]`
	pullRequestsGraphQLJSON   = `{"data":{"repository":{"pullRequests":{"nodes":[{"number":7,"title":"Add feature","state":"OPEN","headRefOid":"abc123","author":{"login":"alice","__typename":"User"}},{"number":8,"title":"Fix bug","state":"OPEN","headRefOid":"def456","author":{"login":"dependabot","__typename":"Bot"}}]}}}}`
)

func TestNormalizers(t *testing.T) {
	reviewsREST := []any{decodedJSON(t, reviewsRESTJSON)}
	reviewsGraphQL := decodedJSON(t, reviewsGraphQLJSON)
	t.Run("reviews via REST → normalized shape", func(t *testing.T) {
		if got := decodedJSON(t, encodeJSON(t, reviewsFromREST(reviewsREST))); !reflect.DeepEqual(got, decodedJSON(t, reviewsRESTJSON)) {
			t.Fatalf("REST reviews = %#v", got)
		}
	})
	t.Run("reviews via GraphQL → same shape ([bot] appended)", func(t *testing.T) {
		if got := reviewsFromGraphQL(reviewsGraphQL); got[1].User.Login != "seal-bot[bot]" {
			t.Fatalf("bot login = %q", got[1].User.Login)
		}
	})

	commentsREST := []any{decodedJSON(t, commentsRESTJSON)}
	commentsGraphQL := decodedJSON(t, commentsGraphQLJSON)
	t.Run("comments via REST → normalized shape", func(t *testing.T) {
		if got := decodedJSON(t, encodeJSON(t, commentsFromREST(commentsREST))); !reflect.DeepEqual(got, decodedJSON(t, commentsRESTJSON)) {
			t.Fatalf("REST comments = %#v", got)
		}
	})
	t.Run("comments via GraphQL → same shape ([bot] appended)", func(t *testing.T) {
		if got := commentsFromGraphQL(commentsGraphQL); got[1].User.Login != "seal-bot[bot]" {
			t.Fatalf("bot login = %q", got[1].User.Login)
		}
	})

	reviewCommentsREST := []any{decodedJSON(t, reviewCommentsRESTJSON)}
	reviewCommentsGraphQL := decodedJSON(t, reviewCommentsGraphQLJSON)
	t.Run("review-comments via REST → normalized shape", func(t *testing.T) {
		if got := decodedJSON(t, encodeJSON(t, reviewCommentsFromREST(reviewCommentsREST))); !reflect.DeepEqual(got, decodedJSON(t, reviewCommentsRESTJSON)) {
			t.Fatalf("REST review comments = %#v", got)
		}
	})
	t.Run("review-comments via GraphQL → same shape ([bot] appended)", func(t *testing.T) {
		if got := reviewCommentsFromGraphQL(reviewCommentsGraphQL); got[1].User.Login != "seal-bot[bot]" {
			t.Fatalf("bot login = %q", got[1].User.Login)
		}
	})

	headREST := decodedJSON(t, headRESTJSON)
	headGraphQL := decodedJSON(t, headGraphQLJSON)
	t.Run("head-sha via REST → bare head SHA", func(t *testing.T) {
		if got := headSHAFromREST(headREST); got != "c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00" {
			t.Fatalf("head SHA = %q", got)
		}
	})
	t.Run("head-sha via GraphQL → same SHA (headRefOid)", func(t *testing.T) {
		if got := headSHAFromGraphQL(headGraphQL); got != headSHAFromREST(headREST) {
			t.Fatalf("head SHA = %q", got)
		}
	})

	checkREST := []any{decodedJSON(t, checkRunsRESTJSON)}
	checkGraphQL := decodedJSON(t, checkRunsGraphQLJSON)
	t.Run("check-runs via REST → normalized lowercase shape", func(t *testing.T) {
		if got := decodedJSON(t, encodeJSON(t, checkRunsFromREST(checkREST))); !reflect.DeepEqual(got, decodedJSON(t, checkRunsRESTJSON)) {
			t.Fatalf("REST check runs = %#v", got)
		}
	})
	t.Run("check-runs via GraphQL → UPPERCASE downcased to same shape", func(t *testing.T) {
		got := checkRunsFromGraphQL(checkGraphQL)
		if got.CheckRuns[0].Status != "completed" || got.CheckRuns[0].Conclusion == nil || *got.CheckRuns[0].Conclusion != "success" {
			t.Fatalf("check run = %#v", got.CheckRuns[0])
		}
	})
	t.Run("check-runs null conclusion stays JSON null (not string)", func(t *testing.T) {
		if got := checkRunsFromGraphQL(checkGraphQL).CheckRuns[1].Conclusion; got != nil {
			t.Fatalf("null conclusion = %q", *got)
		}
	})

	pullRequestsREST := []any{decodedJSON(t, pullRequestsRESTJSON)}
	pullRequestsGraphQL := decodedJSON(t, pullRequestsGraphQLJSON)
	t.Run("pr-list via REST → normalized shape", func(t *testing.T) {
		if got := decodedJSON(t, encodeJSON(t, pullRequestsFromREST(pullRequestsREST))); !reflect.DeepEqual(got, decodedJSON(t, pullRequestsRESTJSON)) {
			t.Fatalf("REST pull requests = %#v", got)
		}
	})
	t.Run("pr-list via GraphQL → same shape (state downcased, [bot] appended)", func(t *testing.T) {
		got := pullRequestsFromGraphQL(pullRequestsGraphQL)
		if got[1].State != "open" || got[1].User.Login != "dependabot[bot]" {
			t.Fatalf("pull request = %#v", got[1])
		}
	})
}

func TestGQLLogin(t *testing.T) {
	for _, test := range []struct {
		name   string
		author any
		want   string
	}{
		{"Bot author gets [bot] suffix", map[string]any{"login": "seal-bot", "__typename": "Bot"}, "seal-bot[bot]"},
		{"User author unchanged", map[string]any{"login": "alice", "__typename": "User"}, "alice"},
		{"missing author → empty login", nil, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := gqlLogin(test.author); got != test.want {
				t.Fatalf("gqlLogin() = %q, want %q", got, test.want)
			}
		})
	}
}

type routeTestState struct {
	now        int64
	cache      string
	cacheValid bool
	logs       []string
	errs       []string
	sleeps     []int64
	calls      [][]string
	gh         func([]string) ghResult
	sequence   []RateLimit
	rateCalls  int
}

func makeRouteDeps(snapshot RateLimit, gh func([]string) ghResult, emptyCache bool, sequence []RateLimit) (routeDeps, *routeTestState) {
	state := &routeTestState{now: 1_000_000, cacheValid: !emptyCache, gh: gh, sequence: sequence}
	if !emptyCache {
		snapshot.FetchedAt = state.now
		state.cache = mustEncodeRateLimit(snapshot)
	}
	deps := routeDeps{
		Env: func(string) (string, bool) { return "", false },
		GH: func(_ context.Context, args []string) ghResult {
			state.calls = append(state.calls, append([]string{}, args...))
			if len(args) > 1 && args[0] == apiCommand && args[1] == "rate_limit" {
				current := snapshot
				if len(state.sequence) > 0 {
					index := state.rateCalls
					if index >= len(state.sequence) {
						index = len(state.sequence) - 1
					}
					current = state.sequence[index]
				}
				state.rateCalls++
				return ghResult{Stdout: mustEncodeRateLimit(current), ExitCode: 0}
			}
			if state.gh != nil {
				return state.gh(args)
			}
			return ghResult{ExitCode: 0}
		},
		Log: func(message string) error { state.logs = append(state.logs, message); return nil },
		Err: func(message string) error { state.errs = append(state.errs, message); return nil },
		Now: func() int64 { return state.now },
		Sleep: func(_ context.Context, duration time.Duration) error {
			seconds := int64(duration / time.Second)
			state.sleeps = append(state.sleeps, seconds)
			state.now += seconds
			return nil
		},
		ReadCache: func() (string, bool) { return state.cache, state.cacheValid },
		WriteCache: func(content string) error {
			state.cache = content
			state.cacheValid = true
			return nil
		},
	}
	return deps, state
}

func TestRunMainRouting(t *testing.T) {
	fixtures := map[string]struct{ rest, graphql string }{
		"reviews":         {reviewsRESTJSON, reviewsGraphQLJSON},
		"comments":        {commentsRESTJSON, commentsGraphQLJSON},
		"review-comments": {reviewCommentsRESTJSON, reviewCommentsGraphQLJSON},
		"head-sha":        {headRESTJSON, headGraphQLJSON},
		"check-runs":      {checkRunsRESTJSON, checkRunsGraphQLJSON},
		"pr-list":         {pullRequestsRESTJSON, pullRequestsGraphQLJSON},
	}
	for _, test := range []struct {
		name    string
		command string
		arg     string
		graphql bool
	}{
		{"reviews via REST", "reviews", "1", false},
		{"reviews via GraphQL", "reviews", "1", true},
		{"comments via REST", "comments", "1", false},
		{"comments via GraphQL", "comments", "1", true},
		{"review-comments via REST", "review-comments", "1", false},
		{"review-comments via GraphQL", "review-comments", "1", true},
		{"head-sha via REST → bare SHA", "head-sha", "1", false},
		{"head-sha via GraphQL → same SHA", "head-sha", "1", true},
		{"check-runs via REST", "check-runs", "abc", false},
		{"check-runs via GraphQL", "check-runs", "abc", true},
		{"pr-list via REST", "pr-list", "", false},
		{"pr-list via GraphQL", "pr-list", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := fixtures[test.command]
			snapshot := testRateLimit(4000, 5000, 300, 5000, 9_999_999_999)
			responseText := fixture.rest
			if test.graphql {
				snapshot = testRateLimit(300, 5000, 4000, 5000, 9_999_999_999)
				responseText = fixture.graphql
			}
			fakeGH := func(args []string) ghResult {
				if len(args) > 1 && args[0] == apiCommand && args[1] == "rate_limit" {
					return ghResult{Stdout: mustEncodeRateLimit(snapshot)}
				}
				return ghResult{Stdout: responseText}
			}
			deps, state := makeRouteDeps(snapshot, fakeGH, false, nil)
			args := []string{test.command}
			if test.arg != "" {
				args = append(args, test.arg)
			}
			args = append(args, "o/r")
			code, err := runMain(t.Context(), deps, args)
			if err != nil || code != 0 {
				t.Fatalf("runMain() = %d, %v", code, err)
			}
			if len(state.logs) != 1 {
				t.Fatalf("logs = %#v", state.logs)
			}
			got := state.logs[0]
			if test.command == "head-sha" {
				if got != "c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00" {
					t.Fatalf("head-sha output = %q", got)
				}
				return
			}
			var normalized any
			if err := json.Unmarshal([]byte(got), &normalized); err != nil {
				t.Fatal(err)
			}
			expected := routeExpectedOutput(t, test.command, fixture.rest)
			if !reflect.DeepEqual(normalized, expected) {
				t.Fatalf("output = %#v; want %#v", normalized, expected)
			}
		})
	}
	t.Run("pick command prints the routing decision", func(t *testing.T) {
		deps, state := makeRouteDeps(testRateLimit(100, 5000, 4000, 5000, 9999), nil, false, nil)
		code, err := runMain(t.Context(), deps, []string{"pick"})
		if err != nil || code != 0 || state.logs[0] != "graphql" {
			t.Fatalf("result = %d, %v, %v", code, err, state.logs)
		}
	})
	t.Run("remaining command prints the bucket count", func(t *testing.T) {
		deps, state := makeRouteDeps(testRateLimit(4321, 5000, 1234, 5000, 9999), nil, false, nil)
		code, err := runMain(t.Context(), deps, []string{"remaining", "graphql"})
		if err != nil || code != 0 || state.logs[0] != "1234" {
			t.Fatalf("result = %d, %v, %v", code, err, state.logs)
		}
	})
}

func routeExpectedOutput(t *testing.T, command, restJSON string) any {
	t.Helper()
	rest := decodedJSON(t, restJSON)
	switch command {
	case "reviews":
		return decodedJSON(t, encodeJSON(t, reviewsFromREST([]any{rest})))
	case "comments":
		return decodedJSON(t, encodeJSON(t, commentsFromREST([]any{rest})))
	case "review-comments":
		return decodedJSON(t, encodeJSON(t, reviewCommentsFromREST([]any{rest})))
	case "check-runs":
		return decodedJSON(t, encodeJSON(t, checkRunsFromREST([]any{rest})))
	case "pr-list":
		return decodedJSON(t, encodeJSON(t, pullRequestsFromREST([]any{rest})))
	default:
		t.Fatalf("unexpected route command %q", command)
		return nil
	}
}

func TestAwaitHeadroom(t *testing.T) {
	t.Run("drained emits the floor warning and returns within the cap", func(t *testing.T) {
		config := Config{Floor: 200, CacheTTL: 15, MaxWait: 1}
		deps, state := makeRouteDeps(testRateLimit(5, 5000, 5, 5000, 9999), nil, false, nil)
		if err := awaitHeadroom(t.Context(), deps, config); err != nil {
			t.Fatal(err)
		}
		if len(state.errs) == 0 || !strings.Contains(state.errs[0], "both buckets < 200") {
			t.Fatalf("warnings = %v", state.errs)
		}
		var total int64
		for _, slept := range state.sleeps {
			if slept > 1 {
				t.Fatalf("sleep %d exceeded max wait", slept)
			}
			total += slept
		}
		if total > 1 {
			t.Fatalf("total wait %d exceeded max wait", total)
		}
	})
	t.Run("headroom present → no wait, no warning", func(t *testing.T) {
		config := Config{Floor: 200, CacheTTL: 15, MaxWait: 1}
		deps, state := makeRouteDeps(testRateLimit(4000, 5000, 4000, 5000, 9999), nil, false, nil)
		if err := awaitHeadroom(t.Context(), deps, config); err != nil {
			t.Fatal(err)
		}
		if len(state.sleeps) != 0 || len(state.errs) != 0 {
			t.Fatalf("warnings = %v, sleeps = %v", state.errs, state.sleeps)
		}
	})
	t.Run("sleeps to the nearest reset then clears when a bucket refills", func(t *testing.T) {
		drained := testRateLimit(5, 5000, 5, 5000, 1_000_050)
		refilled := testRateLimit(4000, 5000, 4000, 5000, 1_000_050)
		config := Config{Floor: 200, CacheTTL: 15, MaxWait: 300}
		deps, state := makeRouteDeps(drained, nil, true, []RateLimit{drained, refilled})
		if err := awaitHeadroom(t.Context(), deps, config); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(state.sleeps, []int64{51}) {
			t.Fatalf("sleeps = %v", state.sleeps)
		}
	})
}

func TestHasNextPage(t *testing.T) {
	for _, test := range []struct {
		name  string
		input any
		want  bool
	}{
		{"hasNextPage detects a true anywhere in the tree", map[string]any{"a": map[string]any{"b": map[string]any{"pageInfo": map[string]any{"hasNextPage": true}}}}, true},
		{"hasNextPage ignores false (jq // drops it)", map[string]any{"pageInfo": map[string]any{"hasNextPage": false}}, false},
		{"hasNextPage over arrays", []any{map[string]any{"x": 1}, map[string]any{"hasNextPage": true}}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := hasNextPage(test.input); got != test.want {
				t.Fatalf("hasNextPage() = %v, want %v", got, test.want)
			}
		})
	}
	t.Run("runMain warns when a GraphQL list hit the 100-item page cap", func(t *testing.T) {
		response := `{"data":{"repository":{"pullRequest":{"reviews":{"pageInfo":{"hasNextPage":true},"nodes":[]}}}}}`
		deps, state := makeRouteDeps(testRateLimit(300, 5000, 4000, 5000, 9999), func([]string) ghResult { return ghResult{Stdout: response} }, false, nil)
		_, err := runMain(t.Context(), deps, []string{"reviews", "1", "o/r"})
		if err != nil {
			t.Fatal(err)
		}
		for _, message := range state.errs {
			if strings.Contains(message, "truncated at the 100-item GraphQL page cap") {
				return
			}
		}
		t.Fatalf("warnings = %v", state.errs)
	})
	t.Run("no truncation warning when hasNextPage is false", func(t *testing.T) {
		deps, state := makeRouteDeps(testRateLimit(300, 5000, 4000, 5000, 9999), func([]string) ghResult { return ghResult{Stdout: `{}`} }, false, nil)
		_, err := runMain(t.Context(), deps, []string{"reviews", "1", "o/r"})
		if err != nil {
			t.Fatal(err)
		}
		for _, message := range state.errs {
			if strings.Contains(message, "truncated") {
				t.Fatalf("unexpected warning %q", message)
			}
		}
	})
}

func TestDispatchExitCodes(t *testing.T) {
	for _, test := range []struct {
		name   string
		args   []string
		code   int
		output string
	}{
		{"unknown command exits 2 and warns", []string{"totally-bogus"}, 2, "unknown command"},
		{"empty command prints help and exits 2", nil, 2, "gh-route <cmd> [args]"},
		{"-h prints help and exits 0", []string{"-h"}, 0, "gh-route <cmd> [args]"},
		{"--help prints help and exits 0", []string{"--help"}, 0, "gh-route <cmd> [args]"},
		{"remaining with no bucket → usage + exit 1", []string{"remaining"}, 1, "usage: gh-route remaining"},
		{"head-sha with no pr → usage + exit 1", []string{"head-sha"}, 1, "usage: gh-route head-sha"},
	} {
		t.Run(test.name, func(t *testing.T) {
			deps, state := makeRouteDeps(testRateLimit(4000, 5000, 4000, 5000, 9999), nil, false, nil)
			code, err := runMain(t.Context(), deps, test.args)
			if err != nil || code != test.code {
				t.Fatalf("runMain() = %d, %v; want %d", code, err, test.code)
			}
			combined := strings.Join(append(state.logs, state.errs...), "\n")
			if !strings.Contains(combined, test.output) {
				t.Fatalf("output %q does not contain %q", combined, test.output)
			}
		})
	}
}

func TestHelpDocumentsEnvironmentAndExitCodes(t *testing.T) {
	command := newRootCommand(t.Context(), func(string) string { return "" }, io.Discard, io.Discard)
	for _, phrase := range []string{"GH_ROUTE_FLOOR", "GH_ROUTE_CACHE_TTL", "GH_ROUTE_MAX_WAIT", "GH_ROUTE_CACHE", "Exit codes:"} {
		if !strings.Contains(command.Long, phrase) {
			t.Errorf("help text missing %q", phrase)
		}
	}
}

func TestPlumbing(t *testing.T) {
	t.Run("parseJsonStream splits concatenated --paginate pages", func(t *testing.T) {
		got := parseJSONStream("[{\"a\":1}]\n[{\"b\":2}]")
		if !reflect.DeepEqual(decodedJSON(t, encodeJSON(t, got)), decodedJSON(t, `[[{"a":1}],[{"b":2}]]`)) {
			t.Fatalf("pages = %#v", got)
		}
	})
	t.Run("parseJsonStream tolerates braces inside strings", func(t *testing.T) {
		if got := parseJSONStream(`[{"body":"a } { b"}]`); len(got) != 1 {
			t.Fatalf("pages = %#v", got)
		}
	})
	t.Run("parseJsonStream skips a balanced-but-invalid page instead of throwing", func(t *testing.T) {
		if got := parseJSONStream("[},{]\n[{\"b\":2}]"); len(got) != 1 {
			t.Fatalf("pages = %#v", got)
		}
	})
	t.Run("slurpAdd flattens array pages (add // [])", func(t *testing.T) {
		pages := []any{decodedJSON(t, `[{"a":1}]`), decodedJSON(t, `[{"b":2},{"c":3}]`)}
		got := slurpAdd(pages)
		want := []map[string]any{{"a": float64(1)}, {"b": float64(2)}, {"c": float64(3)}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("slurped = %#v, want %#v", got, want)
		}
	})
	t.Run("splitRepo splits owner/name", func(t *testing.T) {
		if got := splitRepo("octocat/hello"); got != (Repo{Owner: "octocat", Name: "hello"}) {
			t.Fatalf("splitRepo() = %#v", got)
		}
	})
	t.Run("readConfig defaults and overrides", func(t *testing.T) {
		environment := map[string]string{}
		getenv := func(key string) (string, bool) { value, ok := environment[key]; return value, ok }
		if got := readConfig(getenv); got != (Config{Floor: 200, CacheTTL: 15, MaxWait: 300}) {
			t.Fatalf("default config = %#v", got)
		}
		environment = map[string]string{"GH_ROUTE_FLOOR": "50", "GH_ROUTE_CACHE_TTL": "5", "GH_ROUTE_MAX_WAIT": "10"}
		if got := readConfig(getenv); got != (Config{Floor: 50, CacheTTL: 5, MaxWait: 10}) {
			t.Fatalf("overridden config = %#v", got)
		}
	})
}
