package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/linearauth"
)

type oauthRewrite struct {
	base http.RoundTripper
	host string
}

func (transport oauthRewrite) RoundTrip(request *http.Request) (*http.Response, error) {
	copy := request.Clone(request.Context())
	copy.URL = &url.URL{Scheme: "http", Host: transport.host, Path: request.URL.Path}
	copy.Host = transport.host
	return transport.base.RoundTrip(copy)
}

type advancingRoundTripper struct {
	base       http.RoundTripper
	afterToken func()
}

func (transport advancingRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	if err == nil && request.URL.Path == "/oauth/token" && transport.afterToken != nil {
		transport.afterToken()
	}
	return response, err
}

func sweepFixture(t *testing.T, ids []string, prior *Cache) (SweepDeps, *[][]string) {
	t.Helper()
	root := t.TempDir()
	workspaces := filepath.Join(root, "workspaces")
	agent := filepath.Join(workspaces, "agent-a")
	if err := os.MkdirAll(agent, 0o700); err != nil {
		t.Fatal(err)
	}
	entries := make([]WatchEntry, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, WatchEntry{ID: id, Watermark: new("2026-10-03T22:00:00.000Z")})
	}
	watchlist := Watchlist{SchemaVersion: "watchlist-v1", Issues: entries}
	encoded, err := json.Marshal(watchlist)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agent, "watchlist.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(root, "state", "cache.json")
	notifyPath := filepath.Join(root, "state", "notify.json")
	if prior != nil {
		if err := writeJSONAtomic(cachePath, *prior); err != nil {
			t.Fatal(err)
		}
	}
	clientID := filepath.Join(root, "id")
	secret := filepath.Join(root, "secret")
	if err := os.WriteFile(clientID, []byte("client-id\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("client-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := [][]string{}
	deps := SweepDeps{IssuePrefixes: examplePrefixes(t), WorkspacesRoot: workspaces, CachePath: cachePath, NotifyPath: notifyPath, Credentials: &linearauth.Credentials{ClientID: "client-id", ClientSecret: "client-secret"}, OwnUserID: "own-user-id", Now: func() time.Time { return time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC) }, RunCotal: func(_ context.Context, _ string, args []string, _ time.Duration) (int, string, error) {
		calls = append(calls, args)
		return 0, "", nil
	}, Stdout: io.Discard, Stderr: io.Discard}
	return deps, &calls
}

func sweepServer(t *testing.T, options map[string]any, callLog *[][]string) (*http.Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		(*callLog) = append(*callLog, []string{request.URL.Path})
		switch request.URL.Path {
		case "/oauth/token":
			serveToken(writer, options)
		case "/oauth/revoke":
			serveRevoke(writer, options)
		case "/graphql":
			serveGraphQL(t, writer, request, options)
		default:
			http.NotFound(writer, request)
		}
	}))
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: oauthRewrite{base: server.Client().Transport, host: serverURL.Host}}
	return client, server
}

func serveToken(writer http.ResponseWriter, options map[string]any) {
	writer.Header().Set("Content-Type", "application/json")
	if status, ok := options["mintStatus"].(int); ok {
		writer.WriteHeader(status)
		body, ok := options["mintBody"].(string)
		if !ok {
			body = `{"error":"denied"}`
		}
		_, _ = io.WriteString(writer, body)
		return
	}
	if body, ok := options["mintBody"].(string); ok {
		_, _ = io.WriteString(writer, body)
		return
	}
	_, _ = io.WriteString(writer, `{"access_token":"test-access-token"}`)
}

func serveRevoke(writer http.ResponseWriter, options map[string]any) {
	if fail, _ := options["revokeFails"].(bool); fail {
		writer.WriteHeader(http.StatusInternalServerError)
	}
}

func serveGraphQL(t *testing.T, writer http.ResponseWriter, request *http.Request, options map[string]any) {
	t.Helper()
	if fail, _ := options["graphqlFail"].(bool); fail {
		writer.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(writer, `{"errors":[{"message":"failed"}]}`)
		return
	}
	if status, ok := options["failGraphqlChunk"].(int); ok {
		calls, _ := options["graphqlCalls"].(int)
		calls++
		options["graphqlCalls"] = calls
		if calls == status {
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(writer, `{"errors":[{"message":"failed chunk"}]}`)
			return
		}
	}
	if fail, _ := options["rateLimited"].(bool); fail {
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(writer, `{"errors":[{"extensions":{"code":"RATELIMITED"}}]}`)
		return
	}
	if fail, _ := options["graphqlErrors"].(bool); fail {
		_, _ = io.WriteString(writer, `{"errors":[{"message":"bad query"}]}`)
		return
	}
	var requestBody struct {
		Query     string `json:"query"`
		Variables struct {
			Numbers []int64 `json:"numbers"`
		} `json:"variables"`
	}
	if err := json.NewDecoder(request.Body).Decode(&requestBody); err != nil {
		t.Errorf("decode request: %v", err)
		return
	}
	if queries, ok := options["queries"].(*[]string); ok {
		*queries = append(*queries, requestBody.Query)
	}
	missing, _ := options["missing"].([]int64)
	issues := make([]any, 0, len(requestBody.Variables.Numbers))
	for _, number := range requestBody.Variables.Numbers {
		if containsNumber(missing, number) {
			continue
		}
		issues = append(issues, responseIssue(number, options))
	}
	hasMore, _ := options["hasMore"].(bool)
	response := map[string]any{"data": map[string]any{"viewer": map[string]any{"id": "app-viewer-id"}, "issues": map[string]any{"nodes": issues, "pageInfo": map[string]any{"hasNextPage": false}}}}
	for _, issue := range issues {
		issueMap, _ := issue.(map[string]any)
		comments, _ := issueMap["comments"].(map[string]any)
		pageInfo, _ := comments["pageInfo"].(map[string]any)
		pageInfo["hasNextPage"] = hasMore
	}
	writer.Header().Set("X-Ratelimit-Requests-Remaining", "4987")
	if omit, _ := options["omitComplexity"].(bool); !omit {
		writer.Header().Set("X-Complexity", "13")
	}
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(response); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func containsNumber(numbers []int64, target int64) bool {
	return slices.Contains(numbers, target)
}

func responseIssue(number int64, options map[string]any) map[string]any {
	comments := []any{map[string]any{"id": "reviewer", "createdAt": "2026-10-03T23:00:00.000Z", "editedAt": nil, "body": "reviewer body", "user": map[string]any{"id": "reviewer-id", "name": "Reviewer"}, "botActor": nil, "externalUser": nil}}
	if configured, ok := options["comments"].([]any); ok {
		comments = configured
	}
	return map[string]any{"identifier": fmt.Sprintf("EX-%d", number), "state": map[string]any{"name": "In Progress"}, "assignee": map[string]any{"name": "Worker"}, "comments": map[string]any{"nodes": comments, "pageInfo": map[string]any{"hasNextPage": false}}}
}

func TestFailedSweepsNotifyAfterConsecutiveFailures(t *testing.T) {
	previous := testCache(nil)
	deps, calls := sweepFixture(t, []string{"EX-1"}, &previous)
	httpCalls := &[][]string{}
	client, server := sweepServer(t, map[string]any{"mintStatus": http.StatusUnauthorized, "mintBody": `{"error":"denied"}`}, httpCalls)
	defer server.Close()
	deps.Client = client
	logger := slog.New(slog.DiscardHandler)
	deps.Now = func() time.Time {
		return time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC).Add(3 * time.Second)
	}
	contract := testContract(t)
	if got := runSweep(t.Context(), deps, logger, contract); got != 1 {
		t.Fatalf("first failure exit=%d", got)
	}
	if len(*calls) != 0 {
		t.Fatalf("first failure calls=%#v", *calls)
	}
	if got := runSweep(t.Context(), deps, logger, contract); got != 1 {
		t.Fatalf("second failure exit=%d", got)
	}
	if len(*calls) != 1 || (*calls)[0][2] != "foreman" {
		t.Fatalf("consecutive failure calls=%#v", *calls)
	}
}

func TestSuccessfulCacheUsesISOTimeAndZeroComplexity(t *testing.T) {
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC).Add(7 * time.Millisecond)
	cache := successfulCache(now, []string{"EX-1"}, newOrderedMap[Issue](), []string{}, []string{}, nil, new(float64(0)), 0)
	encoded, err := json.Marshal(cache)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schemaVersion":"linear-comments-v1","status":"ok","fetchedAt":"2026-10-04T00:00:00.007Z","attemptedAt":"2026-10-04T00:00:00.007Z","error":null,"errorKind":null,"rateLimitRemaining":null,"complexity":0,"ownAuthorIds":[],"watched":["EX-1"],"missing":["EX-1"],"warnings":[],"revokeFailures":0,"issues":{}}`
	if string(encoded) != want {
		t.Fatalf("cache bytes = %s, want %s", encoded, want)
	}
}

func TestFailedAndEmptySweepsWriteISOMillisecondTimes(t *testing.T) {
	now := time.Date(2026, 10, 4, 0, 0, 0, 7_654_321, time.UTC)
	const want = "2026-10-04T00:00:00.007Z"
	t.Run("failed sweep", func(t *testing.T) {
		deps, _ := sweepFixture(t, []string{"EX-1"}, nil)
		client, server := sweepServer(t, map[string]any{"mintStatus": http.StatusUnauthorized, "mintBody": `{"error":"denied"}`}, &[][]string{})
		defer server.Close()
		deps.Client = client
		deps.Now = func() time.Time { return now }
		if got := runSweep(t.Context(), deps, slog.New(slog.DiscardHandler), testContract(t)); got != 1 {
			t.Fatalf("sweep exit=%d", got)
		}
		assertCacheTimes(t, deps.CachePath, want, false)
	})
	t.Run("empty watchlist", func(t *testing.T) {
		deps, _ := sweepFixture(t, []string{}, nil)
		deps.Now = func() time.Time { return now }
		if got := runSweep(t.Context(), deps, slog.New(slog.DiscardHandler), testContract(t)); got != 0 {
			t.Fatalf("sweep exit=%d", got)
		}
		assertCacheTimes(t, deps.CachePath, want, true)
	})
}

func assertCacheTimes(t *testing.T, path, want string, fetched bool) {
	t.Helper()
	cache, err := readCache(path)
	if err != nil || cache == nil {
		t.Fatalf("read cache: %v", err)
	}
	if cache.AttemptedAt != want {
		t.Fatalf("attemptedAt = %q, want %q", cache.AttemptedAt, want)
	}
	if fetched && (cache.FetchedAt == nil || *cache.FetchedAt != want) {
		t.Fatalf("fetchedAt = %v, want %q", cache.FetchedAt, want)
	}
}

func TestSweepMissingComplexityHeaderDefaultsToZero(t *testing.T) {
	deps, _ := sweepFixture(t, []string{"EX-1"}, nil)
	client, server := sweepServer(t, map[string]any{"omitComplexity": true}, &[][]string{})
	defer server.Close()
	deps.Client = client
	if got := runSweep(t.Context(), deps, slog.New(slog.DiscardHandler), testContract(t)); got != 0 {
		t.Fatalf("sweep exit=%d", got)
	}
	cache, err := readCache(deps.CachePath)
	if err != nil || cache == nil || cache.Complexity == nil || *cache.Complexity != 0 {
		t.Fatalf("cache complexity = %#v, err=%v", cache, err)
	}
}

func TestMintHTTP504IsNotTimeout(t *testing.T) {
	deps, _ := sweepFixture(t, []string{"EX-1"}, nil)
	client, server := sweepServer(t, map[string]any{"mintStatus": http.StatusGatewayTimeout, "mintBody": "Gateway Timeout"}, &[][]string{})
	defer server.Close()
	deps.Client = client
	if got := runSweep(t.Context(), deps, slog.New(slog.DiscardHandler), testContract(t)); got != 1 {
		t.Fatalf("sweep exit=%d", got)
	}
	cache, err := readCache(deps.CachePath)
	if err != nil || cache == nil || cache.ErrorKind == nil || *cache.ErrorKind != ErrorHTTP {
		t.Fatalf("504 cache=%#v err=%v", cache, err)
	}
}

func TestMintMalformedScopeMetadataStillMints(t *testing.T) {
	for _, scope := range []string{`{"unexpected":true}`, `42`, `null`} {
		t.Run(scope, func(t *testing.T) {
			deps, _ := sweepFixture(t, []string{"EX-1"}, nil)
			client, server := sweepServer(t, map[string]any{"mintBody": `{"access_token":"test-access-token","scope":` + scope + `}`}, &[][]string{})
			defer server.Close()
			deps.Client = client
			if got := runSweep(t.Context(), deps, slog.New(slog.DiscardHandler), testContract(t)); got != 0 {
				t.Fatalf("sweep exit=%d for scope %s", got, scope)
			}
		})
	}
}

func TestRunSweep(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	contract := testContract(t)
	t.Run("queries", func(t *testing.T) { testSweepQueries(t, logger, contract) })
	t.Run("prerequisites", func(t *testing.T) { testSweepPrerequisites(t, logger, contract) })
	t.Run("failure handling", func(t *testing.T) { testSweepFailures(t, logger, contract) })
	t.Run("deadline", func(t *testing.T) { testSweepDeadline(t, logger, contract) })
}

func testSweepQueries(t *testing.T, logger *slog.Logger, contract Contract) {
	t.Helper()
	t.Run("mints reads in groups of fifty and revokes after final query", func(t *testing.T) {
		ids := make([]string, 60)
		for index := range ids {
			ids[index] = fmt.Sprintf("EX-%d", index+1)
		}
		deps, calls := sweepFixture(t, ids, nil)
		clientCalls := [][]string{}
		client, server := sweepServer(t, map[string]any{}, &clientCalls)
		defer server.Close()
		deps.Client = client
		var stdout, stderr strings.Builder
		deps.Stdout = &stdout
		deps.Stderr = &stderr
		if got := runSweep(t.Context(), deps, logger, contract); got != 0 {
			t.Fatalf("sweep exit=%d stderr=%s", got, stderr.String())
		}
		if len(clientCalls) != 4 || clientCalls[0][0] != "/oauth/token" || clientCalls[1][0] != "/graphql" || clientCalls[2][0] != "/graphql" || clientCalls[3][0] != "/oauth/revoke" {
			t.Fatalf("requests = %#v", clientCalls)
		}
		if !strings.HasPrefix(stdout.String(), "linear-comment-poller: sweep ok issues=60 requests=2") {
			t.Fatalf("success line = %q", stdout.String())
		}
		if len(*calls) != 1 || !strings.HasSuffix((*calls)[0][3], "run linear-comments") {
			t.Fatalf("unexpected notifications: %#v", *calls)
		}
		cache, err := readCache(deps.CachePath)
		if err != nil || cache == nil || cache.Complexity == nil || *cache.Complexity != 26 {
			t.Fatalf("cache = %#v err=%v", cache, err)
		}
	})
	t.Run("sends one wake for Reviewer reply and excludes agent reply", func(t *testing.T) {
		deps, calls := sweepFixture(t, []string{"EX-1"}, nil)
		commentList := []any{map[string]any{"id": "reviewer", "createdAt": "2026-10-03T23:30:00.000Z", "editedAt": nil, "body": "reviewer body", "user": map[string]any{"id": "reviewer-id", "name": "Pat Reviewer"}, "botActor": nil, "externalUser": nil}, map[string]any{"id": "agent", "createdAt": "2026-10-03T23:31:00.000Z", "editedAt": nil, "body": "agent body", "user": map[string]any{"id": "own-user-id", "name": "Worker"}, "botActor": nil, "externalUser": nil}}
		client, server := sweepServer(t, map[string]any{"comments": commentList}, &[][]string{})
		defer server.Close()
		deps.Client = client
		deps.OwnUserID = "own-user-id"
		if got := runSweep(t.Context(), deps, slog.New(slog.DiscardHandler), contract); got != 0 {
			t.Fatalf("sweep exit=%d", got)
		}
		if len(*calls) != 1 || (*calls)[0][2] != "agent-a" || !strings.Contains((*calls)[0][3], "EX-1: 1 new comment") {
			t.Fatalf("cotal calls=%#v", *calls)
		}
	})
	t.Run("partial miss preserves result and reports missing issue", func(t *testing.T) {
		deps, calls := sweepFixture(t, []string{"EX-1", "EX-2"}, nil)
		client, server := sweepServer(t, map[string]any{"missing": []int64{2}}, &[][]string{})
		defer server.Close()
		deps.Client = client
		var stdout, stderr strings.Builder
		deps.Stdout = &stdout
		deps.Stderr = &stderr
		if got := runSweep(t.Context(), deps, slog.New(slog.DiscardHandler), contract); got != 0 {
			t.Fatalf("sweep exit=%d", got)
		}
		cache, err := readCache(deps.CachePath)
		if err != nil || cache == nil || len(cache.Missing) != 1 || cache.Missing[0] != "EX-2" {
			t.Fatalf("cache=%#v err=%v", cache, err)
		}
		if !strings.Contains(stderr.String(), "EX-2") || !strings.HasPrefix(stdout.String(), contract.SuccessPrefix) {
			t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
		}
		if len(*calls) != 1 {
			t.Fatalf("notifications=%#v", *calls)
		}
	})
}

func testSweepPrerequisites(t *testing.T, logger *slog.Logger, contract Contract) {
	t.Helper()
	t.Run("empty watchlist succeeds without credentials", func(t *testing.T) {
		deps, _ := sweepFixture(t, nil, nil)
		deps.Credentials = nil
		deps.OwnUserID = ""
		deps.Client = nil
		var stdout strings.Builder
		deps.Stdout = &stdout
		if got := runSweep(t.Context(), deps, logger, contract); got != 0 {
			t.Fatalf("empty sweep exit=%d", got)
		}
		if !strings.Contains(stdout.String(), "issues=0 requests=0") {
			t.Fatalf("output=%q", stdout.String())
		}
	})
	t.Run("missing credentials and identity fail before request", func(t *testing.T) {
		for _, missing := range []string{"credentials", "own-user"} {
			t.Run(missing, func(t *testing.T) {
				deps, _ := sweepFixture(t, []string{"EX-1"}, nil)
				if missing == "credentials" {
					deps.Credentials = nil
				} else {
					deps.OwnUserID = ""
				}
				if got := runSweep(t.Context(), deps, logger, contract); got != 1 {
					t.Fatalf("exit=%d", got)
				}
				cache, err := readCache(deps.CachePath)
				if err != nil || cache == nil || cache.ErrorKind == nil || *cache.ErrorKind != ErrorNoCredential {
					t.Fatalf("cache=%#v err=%v", cache, err)
				}
			})
		}
	})
}

func testSweepFailures(t *testing.T, logger *slog.Logger, contract Contract) {
	t.Helper()
	t.Run("token failure maps HTTP 401 to auth", func(t *testing.T) { testSweepTokenFailure(t, logger, contract) })
	t.Run("GraphQL failure revokes and preserves old data", func(t *testing.T) { testSweepGraphQLFailure(t, logger, contract) })
	t.Run("revoke failure increments count", func(t *testing.T) { testSweepRevokeFailure(t, logger, contract) })
	t.Run("revoke limit marks stale auth", func(t *testing.T) { testSweepRevokeLimit(t, contract) })
	t.Run("no returned issues fails as missing", func(t *testing.T) { testSweepMissingIssues(t, logger, contract) })
	t.Run("cache read error leaves directory intact", func(t *testing.T) { testSweepCacheReadFailure(t, logger, contract) })
}

func testSweepTokenFailure(t *testing.T, logger *slog.Logger, contract Contract) {
	t.Helper()
	deps, _ := sweepFixture(t, []string{"EX-1"}, nil)
	client, server := sweepServer(t, map[string]any{"mintStatus": 401}, &[][]string{})
	defer server.Close()
	deps.Client = client
	if got := runSweep(t.Context(), deps, logger, contract); got != 1 {
		t.Fatalf("exit=%d", got)
	}
	cache, err := readCache(deps.CachePath)
	if err != nil || cache == nil || cache.ErrorKind == nil || *cache.ErrorKind != ErrorAuth || !strings.Contains(*cache.Error, "HTTP 401") || strings.Contains(*cache.Error, "denied") {
		t.Fatalf("cache=%#v err=%v", cache, err)
	}
}

func testSweepGraphQLFailure(t *testing.T, logger *slog.Logger, contract Contract) {
	t.Helper()
	previous := testCache(nil)
	previous.Status = "stale"
	previous.RevokeFailures = 7
	deps, _ := sweepFixture(t, []string{"EX-1"}, &previous)
	clientCalls := [][]string{}
	client, server := sweepServer(t, map[string]any{"graphqlFail": true, "revokeFails": true}, &clientCalls)
	defer server.Close()
	deps.Client = client
	if got := runSweep(t.Context(), deps, logger, contract); got != 1 {
		t.Fatalf("exit=%d", got)
	}
	cache, err := readCache(deps.CachePath)
	if err != nil || cache == nil || cache.Status != "stale" || cache.ErrorKind == nil || *cache.ErrorKind != ErrorHTTP || cache.RevokeFailures != 8 {
		t.Fatalf("cache=%#v err=%v", cache, err)
	}
	if len(clientCalls) != 3 || clientCalls[2][0] != "/oauth/revoke" {
		t.Fatalf("calls=%#v", clientCalls)
	}
}

func testSweepRevokeFailure(t *testing.T, logger *slog.Logger, contract Contract) {
	t.Helper()
	deps, _ := sweepFixture(t, []string{"EX-1"}, nil)
	client, server := sweepServer(t, map[string]any{"revokeFails": true}, &[][]string{})
	defer server.Close()
	deps.Client = client
	if got := runSweep(t.Context(), deps, logger, contract); got != 0 {
		t.Fatalf("exit=%d", got)
	}
	cache, err := readCache(deps.CachePath)
	if err != nil || cache == nil || cache.RevokeFailures != 1 || len(cache.Warnings) != 1 {
		t.Fatalf("cache=%#v err=%v", cache, err)
	}
}

func testSweepRevokeLimit(t *testing.T, contract Contract) {
	t.Helper()
	previous := testCache(nil)
	previous.RevokeFailures = 95
	deps, _ := sweepFixture(t, []string{"EX-1"}, &previous)
	client, server := sweepServer(t, map[string]any{"revokeFails": true}, &[][]string{})
	defer server.Close()
	deps.Client = client
	if got := runSweep(t.Context(), deps, slog.New(slog.DiscardHandler), contract); got != 1 {
		t.Fatalf("exit=%d", got)
	}
	cache, err := readCache(deps.CachePath)
	if err != nil || cache == nil || cache.ErrorKind == nil || *cache.ErrorKind != ErrorAuth || cache.RevokeFailures != 96 {
		t.Fatalf("cache=%#v err=%v", cache, err)
	}
}

func testSweepMissingIssues(t *testing.T, logger *slog.Logger, contract Contract) {
	t.Helper()
	deps, _ := sweepFixture(t, []string{"EX-1"}, nil)
	client, server := sweepServer(t, map[string]any{"missing": []int64{1}}, &[][]string{})
	defer server.Close()
	deps.Client = client
	if got := runSweep(t.Context(), deps, logger, contract); got != 1 {
		t.Fatalf("exit=%d", got)
	}
	cache, err := readCache(deps.CachePath)
	if err != nil || cache == nil || cache.ErrorKind == nil || *cache.ErrorKind != ErrorMissing {
		t.Fatalf("cache=%#v err=%v", cache, err)
	}
}

func testSweepCacheReadFailure(t *testing.T, logger *slog.Logger, contract Contract) {
	t.Helper()
	deps, _ := sweepFixture(t, nil, nil)
	if err := os.MkdirAll(deps.CachePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := runSweep(t.Context(), deps, logger, contract); got != 1 {
		t.Fatalf("exit=%d", got)
	}
	info, err := os.Stat(deps.CachePath)
	if err != nil || !info.IsDir() {
		t.Fatalf("cache path info=%v err=%v", info, err)
	}
}

func testSweepDeadline(t *testing.T, logger *slog.Logger, contract Contract) {
	t.Helper()
	previous := testCache(nil)
	deps, _ := sweepFixture(t, []string{"EX-1"}, &previous)
	baseNow := deps.Now
	currentTime := baseNow()
	deps.Now = func() time.Time { return currentTime }
	clientCalls := [][]string{}
	client, _ := sweepServer(t, map[string]any{}, &clientCalls)
	deps.Client = &http.Client{Transport: advancingRoundTripper{base: client.Transport, afterToken: func() { currentTime = baseNow().Add(sweepDeadline) }}}
	if got := runSweep(t.Context(), deps, logger, contract); got != 1 {
		t.Fatalf("exit=%d", got)
	}
	cache, err := readCache(deps.CachePath)
	if err != nil || cache.ErrorKind == nil || *cache.ErrorKind != ErrorDeadline {
		t.Fatalf("cache=%#v err=%v", cache, err)
	}
	if !reflect.DeepEqual(clientCalls, [][]string{{"/oauth/token"}, {"/oauth/revoke"}}) {
		t.Fatalf("calls=%#v", clientCalls)
	}
}

func TestRunSweepParity(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	contract := testContract(t)
	t.Run("empty watchlist retains prior author IDs and revoke count", func(t *testing.T) {
		previous := testCache(nil)
		previous.RevokeFailures = 3
		deps, _ := sweepFixture(t, nil, &previous)
		deps.Credentials = nil
		deps.OwnUserID = ""
		deps.Client = nil
		if got := runSweep(t.Context(), deps, logger, contract); got != 0 {
			t.Fatalf("empty sweep exit = %d", got)
		}
		cache, err := readCache(deps.CachePath)
		if err != nil || cache == nil || cache.RevokeFailures != 3 || !reflect.DeepEqual(cache.OwnAuthorIDs, previous.OwnAuthorIDs) || len(cache.Issues.keys) != 0 {
			t.Fatalf("empty sweep cache = %#v err=%v", cache, err)
		}
	})
	t.Run("agents app user comments never wake an agent", func(t *testing.T) {
		deps, calls := sweepFixture(t, []string{"EX-1"}, nil)
		deps.AgentsAppUserID = " agents-app-id\n"
		comments := []any{map[string]any{"id": "app-reply", "createdAt": "2026-10-03T23:30:00.000Z", "editedAt": nil, "body": "app body", "user": map[string]any{"id": "agents-app-id", "name": "Rigel Agents"}, "botActor": nil, "externalUser": nil}}
		client, server := sweepServer(t, map[string]any{"comments": comments}, &[][]string{})
		defer server.Close()
		deps.Client = client
		if got := runSweep(t.Context(), deps, logger, contract); got != 0 {
			t.Fatalf("sweep exit = %d", got)
		}
		if len(*calls) != 0 {
			t.Fatalf("app user comments sent wakes: %#v", *calls)
		}
		cache, err := readCache(deps.CachePath)
		if err != nil || cache == nil || !reflect.DeepEqual(cache.OwnAuthorIDs, []string{"own-user-id", "agents-app-id", "app-viewer-id"}) {
			t.Fatalf("own IDs = %#v err=%v", cache, err)
		}
	})
	t.Run("mint failures are sanitized", func(t *testing.T) { testMintFailuresAreSanitized(t, logger, contract) })
	t.Run("GraphQL transport failures are reported as network errors", func(t *testing.T) {
		deps, _ := sweepFixture(t, []string{"EX-1"}, nil)
		deps.Client = &http.Client{Transport: sweepGraphQLErrorTransport{}}
		if got := runSweep(t.Context(), deps, logger, contract); got != 1 {
			t.Fatalf("sweep exit=%d", got)
		}
		cache, err := readCache(deps.CachePath)
		if err != nil || cache == nil || cache.ErrorKind == nil || *cache.ErrorKind != ErrorNetwork {
			t.Fatalf("network failure cache = %#v err=%v", cache, err)
		}
	})
	t.Run("failed second chunk preserves previous cache and revokes once", func(t *testing.T) {
		testFailedSecondChunkPreservesCache(t, logger)
	})
	t.Run("rate limit and GraphQL errors keep previous cache without retry", func(t *testing.T) {
		testGraphQLErrorsPreserveCache(t, logger)
	})
	t.Run("successful revoke resets a prior failure count", func(t *testing.T) {
		testSuccessfulRevokeResetsCount(t, logger)
	})
}

func testMintFailuresAreSanitized(t *testing.T, logger *slog.Logger, contract Contract) {
	t.Helper()
	for _, test := range []struct {
		name   string
		option map[string]any
		kind   ErrorKind
		text   string
	}{
		{name: "network", kind: ErrorNetwork, text: "TypeError ENOTFOUND"},
		{name: "http", option: map[string]any{"mintStatus": 503}, kind: ErrorHTTP, text: "HTTP 503"},
		{name: "bad token shape", option: map[string]any{"mintBody": `{"token_type":"secret-shape"}`}, kind: ErrorBadShape, text: "invalid token response"},
	} {
		t.Run(test.name, func(t *testing.T) {
			deps, _ := sweepFixture(t, []string{"EX-1"}, nil)
			var client *http.Client
			if test.name == "network" {
				client = &http.Client{Transport: sweepRoundTripError{err: &net.DNSError{IsNotFound: true}}}
			} else {
				var server *httptest.Server
				client, server = sweepServer(t, test.option, &[][]string{})
				defer server.Close()
			}
			deps.Client = client
			var stdout, stderr strings.Builder
			deps.Stdout = &stdout
			deps.Stderr = &stderr
			if got := runSweep(t.Context(), deps, logger, contract); got != 1 {
				t.Fatalf("sweep exit=%d", got)
			}
			cache, err := readCache(deps.CachePath)
			if err != nil || cache == nil || cache.ErrorKind == nil || *cache.ErrorKind != test.kind || cache.Error == nil || !strings.Contains(*cache.Error, test.text) {
				t.Fatalf("failure cache = %#v err=%v", cache, err)
			}
			if strings.Contains(stdout.String()+stderr.String(), "secret-shape") || strings.Contains(stdout.String()+stderr.String(), "upstream") || strings.Contains(stdout.String()+stderr.String(), "client-secret") {
				t.Fatal("failure exposed response or credential content")
			}
		})
	}
}

func testFailedSecondChunkPreservesCache(t *testing.T, logger *slog.Logger) {
	t.Helper()
	ids := make([]string, 60)
	for index := range ids {
		ids[index] = fmt.Sprintf("EX-%d", index+1)
	}
	previous := testCache(nil)
	previous.Status = "stale"
	previous.Watched = []string{"EX-90", "EX-91"}
	previous.Missing = []string{"EX-91"}
	previous.RevokeFailures = 7
	previous.Complexity = new(float64(73))
	deps, _ := sweepFixture(t, ids, &previous)
	clientCalls := [][]string{}
	client, server := sweepServer(t, map[string]any{"failGraphqlChunk": 2, "revokeFails": true}, &clientCalls)
	defer server.Close()
	deps.Client = client
	if got := runSweep(t.Context(), deps, logger, testContract(t)); got != 1 {
		t.Fatalf("sweep exit=%d", got)
	}
	cache, err := readCache(deps.CachePath)
	if err != nil || cache == nil || cache.Status != "stale" || cache.ErrorKind == nil || *cache.ErrorKind != ErrorHTTP || cache.RevokeFailures != 8 || cache.Complexity == nil || *cache.Complexity != 73 || !reflect.DeepEqual(cache.Watched, previous.Watched) || !reflect.DeepEqual(cache.Missing, previous.Missing) {
		t.Fatalf("failed chunk cache=%#v err=%v", cache, err)
	}
	if len(clientCalls) != 4 || clientCalls[0][0] != "/oauth/token" || clientCalls[1][0] != "/graphql" || clientCalls[2][0] != "/graphql" || clientCalls[3][0] != "/oauth/revoke" {
		t.Fatalf("requests=%#v", clientCalls)
	}
	if _, ok := cache.Issues.get("EX-1"); !ok {
		t.Fatal("previous issue data was lost")
	}
}

func testGraphQLErrorsPreserveCache(t *testing.T, logger *slog.Logger) {
	t.Helper()
	for _, test := range []struct {
		name   string
		option map[string]any
		kind   ErrorKind
	}{
		{name: "RATELIMITED", option: map[string]any{"rateLimited": true}, kind: ErrorRateLimited},
		{name: "GraphQL errors", option: map[string]any{"graphqlErrors": true}, kind: ErrorGraphQL},
	} {
		t.Run(test.name, func(t *testing.T) {
			previous := testCache(nil)
			previous.Status = "stale"
			deps, _ := sweepFixture(t, []string{"EX-1"}, &previous)
			clientCalls := [][]string{}
			client, server := sweepServer(t, test.option, &clientCalls)
			defer server.Close()
			deps.Client = client
			if got := runSweep(t.Context(), deps, logger, testContract(t)); got != 1 {
				t.Fatalf("sweep exit=%d", got)
			}
			cache, err := readCache(deps.CachePath)
			if err != nil || cache == nil || cache.Status != "stale" || cache.ErrorKind == nil || *cache.ErrorKind != test.kind {
				t.Fatalf("cache=%#v err=%v", cache, err)
			}
			if len(clientCalls) != 3 || clientCalls[2][0] != "/oauth/revoke" {
				t.Fatalf("requests=%#v", clientCalls)
			}
		})
	}
}

func testSuccessfulRevokeResetsCount(t *testing.T, logger *slog.Logger) {
	t.Helper()
	previous := testCache(nil)
	previous.RevokeFailures = 5
	deps, _ := sweepFixture(t, []string{"EX-1"}, &previous)
	client, server := sweepServer(t, map[string]any{}, &[][]string{})
	defer server.Close()
	deps.Client = client
	if got := runSweep(t.Context(), deps, logger, testContract(t)); got != 0 {
		t.Fatalf("sweep exit=%d", got)
	}
	cache, err := readCache(deps.CachePath)
	if err != nil || cache == nil || cache.RevokeFailures != 0 {
		t.Fatalf("cache=%#v err=%v", cache, err)
	}
}

func TestSweepTokenTimeoutAndBodyNetworkFailure(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	contract := testContract(t)
	t.Run("token timeout is classified and revocation is skipped", func(t *testing.T) {
		deps, _ := sweepFixture(t, []string{"EX-1"}, nil)
		deps.Client = &http.Client{Transport: sweepRoundTripError{err: context.DeadlineExceeded}}
		if got := runSweep(t.Context(), deps, logger, contract); got != 1 {
			t.Fatalf("sweep exit=%d", got)
		}
		cache, err := readCache(deps.CachePath)
		if err != nil || cache == nil || cache.ErrorKind == nil || *cache.ErrorKind != ErrorTimeout {
			t.Fatalf("timeout cache=%#v err=%v", cache, err)
		}
	})
	t.Run("token body read reset is classified as network failure", func(t *testing.T) {
		deps, _ := sweepFixture(t, []string{"EX-1"}, nil)
		deps.Client = &http.Client{Transport: sweepTokenBodyErrorTransport{body: errorReadCloser{err: &net.OpError{Err: syscall.ECONNRESET}}}}
		if got := runSweep(t.Context(), deps, logger, contract); got != 1 {
			t.Fatalf("sweep exit=%d", got)
		}
		cache, err := readCache(deps.CachePath)
		if err != nil || cache == nil || cache.ErrorKind == nil || *cache.ErrorKind != ErrorNetwork || cache.Error == nil || !strings.Contains(*cache.Error, "TypeError ECONNRESET") {
			t.Fatalf("token body cache=%#v error=%q err=%v", cache, errorString(cache.Error), err)
		}
	})
}

type sweepTokenBodyErrorTransport struct{ body io.ReadCloser }

func (transport sweepTokenBodyErrorTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Path != "/oauth/token" {
		return nil, errors.New("unexpected token request path")
	}
	body := transport.body
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, Request: request}, nil
}

type errorReadCloser struct{ err error }

func (reader errorReadCloser) Read([]byte) (int, error) { return 0, reader.err }
func (errorReadCloser) Close() error                    { return nil }

type sweepRoundTripError struct{ err error }

func (transport sweepRoundTripError) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, transport.err
}

type sweepGraphQLErrorTransport struct{}

func (sweepGraphQLErrorTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Path == "/oauth/token" {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"access_token":"test"}`)), Request: request}, nil
	}
	return nil, errors.New("connection refused")
}

func TestRunSweepQueriesOnlyCanonicalTeam(t *testing.T) {
	deps, _ := sweepFixture(t, []string{"demo-3", "EX-4"}, nil)
	queries := []string{}
	client, server := sweepServer(t, map[string]any{"queries": &queries}, &[][]string{})
	defer server.Close()
	deps.Client = client
	if got := runSweep(t.Context(), deps, slog.New(slog.DiscardHandler), testContract(t)); got != 0 {
		t.Fatalf("sweep exit=%d", got)
	}
	if len(queries) != 1 || !strings.Contains(queries[0], `team: { key: { eq: "EX" } }`) || strings.Contains(queries[0], "DEMO") {
		t.Fatalf("queries = %q", queries)
	}
	cache, err := readCache(deps.CachePath)
	if err != nil || cache == nil || strings.Join(cache.Watched, ",") != "EX-3,EX-4" {
		t.Fatalf("cache = %#v err=%v", cache, err)
	}
}

type failOnRequestTransport struct{ t *testing.T }

func (transport failOnRequestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.t.Errorf("unexpected request to %s", request.URL)
	return nil, errors.New("unexpected request")
}

func TestSweepRequiresIssuePrefixBeforeNetwork(t *testing.T) {
	previous := http.DefaultTransport
	http.DefaultTransport = failOnRequestTransport{t: t}
	t.Cleanup(func() { http.DefaultTransport = previous })
	root := t.TempDir()
	cachePath := filepath.Join(root, "cache.json")
	getenv := func(key string) string {
		switch key {
		case "LINEAR_COMMENTS_CACHE":
			return cachePath
		case "LINEAR_COMMENTS_NOTIFY":
			return filepath.Join(root, "notify.json")
		case "WAVE_WORKSPACES":
			return root
		}
		return ""
	}
	for _, args := range [][]string{{"sweep"}, {"--issue-prefix", "", "sweep"}} {
		var stdout, stderr bytes.Buffer
		if got := runWithArgs(t.Context(), getenv, args, &stdout, &stderr); got != 2 {
			t.Fatalf("args %q exit=%d stderr=%q", args, got, stderr.String())
		}
		if !strings.Contains(stderr.String(), "at least one team key") {
			t.Fatalf("args %q stderr=%q", args, stderr.String())
		}
	}
	if _, err := os.Stat(cachePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache written without an issue prefix: err=%v", err)
	}
}
