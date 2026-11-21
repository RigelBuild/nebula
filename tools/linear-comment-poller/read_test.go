package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFreshness(t *testing.T) {
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	contract := testContract(t)
	for _, test := range []struct {
		name            string
		cache           *Cache
		stale, fallback bool
	}{
		{name: "fresh", cache: new(testCache(func(cache *Cache) { cache.FetchedAt = new("2026-10-03T23:31:00.000Z") }))},
		{name: "stale by age", cache: new(testCache(func(cache *Cache) { cache.FetchedAt = new("2026-10-03T23:29:00.000Z") })), stale: true},
		{name: "future timestamp", cache: new(testCache(func(cache *Cache) { cache.FetchedAt = new("2026-10-04T00:01:00.000Z") })), stale: true},
		{name: "stale status rate limit", cache: new(testCache(func(cache *Cache) {
			cache.Status = "stale"
			cache.ErrorKind = kindPointer(ErrorRateLimited)
			cache.RateLimitRemaining = new(float64(0))
		})), stale: true},
		{name: "over fallback", cache: new(testCache(func(cache *Cache) { cache.Status = "stale"; cache.FetchedAt = new("2026-10-03T21:59:00.000Z") })), stale: true, fallback: true},
		{name: "under fallback", cache: new(testCache(func(cache *Cache) { cache.Status = "stale"; cache.FetchedAt = new("2026-10-03T22:01:00.000Z") })), stale: true},
		{name: "no cache", stale: true, fallback: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := freshness(test.cache, now, contract)
			if got.Stale != test.stale || got.Fallback != test.fallback {
				t.Fatalf("freshness = %#v", got)
			}
		})
	}
}

func TestFreshnessRenderingParity(t *testing.T) {
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	contract := testContract(t)
	stale := testCache(func(cache *Cache) {
		cache.Status = "stale"
		cache.ErrorKind = kindPointer(ErrorRateLimited)
		cache.RateLimitRemaining = new(float64(0))
	})
	got := freshness(&stale, now, contract)
	if !got.Stale || !strings.Contains(got.Lines[0], "RATELIMITED (remaining 0)") {
		t.Fatalf("rate-limit stale line = %#v", got)
	}
	overFallback := testCache(func(cache *Cache) {
		cache.Status = "stale"
		cache.FetchedAt = new("2026-10-03T21:59:00.000Z")
	})
	got = freshness(&overFallback, now, contract)
	if !got.Fallback || !strings.Contains(strings.Join(got.Lines, "\n"), fallbackLine) {
		t.Fatalf("over-fallback freshness = %#v", got)
	}
	underFallback := testCache(func(cache *Cache) {
		cache.Status = "stale"
		cache.FetchedAt = new("2026-10-03T22:01:00.000Z")
	})
	got = freshness(&underFallback, now, contract)
	if got.Fallback || strings.Contains(strings.Join(got.Lines, "\n"), fallbackLine) {
		t.Fatalf("under-fallback freshness = %#v", got)
	}
	got = freshness(nil, now, contract)
	if !got.Stale || !got.Fallback || !strings.Contains(strings.Join(got.Lines, "\n"), "last good never") {
		t.Fatalf("missing cache freshness = %#v", got)
	}
}

func TestRunRead(t *testing.T) {
	root := t.TempDir()
	watchlistPath := filepath.Join(root, "watchlist.json")
	cachePath := filepath.Join(root, "cache.json")
	watchlist := Watchlist{SchemaVersion: "watchlist-v1", Issues: []WatchEntry{}}
	encodedWatchlist, err := json.Marshal(watchlist)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(watchlistPath, encodedWatchlist, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	contract := testContract(t)
	for _, test := range []struct {
		name     string
		cache    *Cache
		expected int
	}{
		{name: "fresh", cache: new(testCache(func(cache *Cache) { cache.FetchedAt = new("2026-10-03T23:30:00.000Z") })), expected: 0},
		{name: "at threshold is fresh", cache: new(testCache(func(cache *Cache) { cache.FetchedAt = new("2026-10-03T23:30:00.000Z") })), expected: 0},
		{name: "past threshold stale", cache: new(testCache(func(cache *Cache) { cache.FetchedAt = new("2026-10-03T23:29:00.000Z") })), expected: 1},
		{name: "stale status", cache: new(testCache(func(cache *Cache) { cache.Status = "stale"; cache.ErrorKind = kindPointer(ErrorHTTP) })), expected: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := writeJSONAtomic(cachePath, test.cache); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			got := runRead([]string{"--watchlist", watchlistPath}, cachePath, now, &stdout, &stderr, contract, examplePrefixes(t))
			if got != test.expected || stderr.Len() != 0 {
				t.Fatalf("read exit=%d stderr=%q", got, stderr.String())
			}
			if test.expected == 0 && !strings.Contains(stdout.String(), "cache: fresh") {
				t.Fatalf("fresh output = %s", stdout.String())
			}
			if test.expected == 1 && !strings.Contains(stdout.String(), "cache: STALE") {
				t.Fatalf("stale output = %s", stdout.String())
			}
		})
	}
	for _, test := range []struct {
		name         string
		args         []string
		content      string
		expectedText string
	}{
		{name: "bad argv", args: []string{"--bad"}, expectedText: "usage"},
		{name: "invalid watchlist", args: []string{"--watchlist", watchlistPath}, content: `{"schemaVersion":"bad"}`, expectedText: "invalid schemaVersion"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.content != "" {
				if err := os.WriteFile(watchlistPath, []byte(test.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			if got := runRead(test.args, cachePath, now, &stdout, &stderr, contract, examplePrefixes(t)); got != 2 || !strings.Contains(stderr.String(), test.expectedText) {
				t.Fatalf("read exit=%d stderr=%q", got, stderr.String())
			}
		})
	}
}

func TestRunPreservesReadExitCodeTwo(t *testing.T) {
	root := t.TempDir()
	watchlistPath := filepath.Join(root, "watchlist.json")
	if err := os.WriteFile(watchlistPath, []byte(`{"schemaVersion":"bad"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "invalid watchlist", args: []string{"--issue-prefix", "EX", "read", "--watchlist", watchlistPath}},
		{name: "unexpected argument", args: []string{"--issue-prefix", "EX", "read", "unexpected"}},
		{name: "unknown flag", args: []string{"--issue-prefix", "EX", "read", "--unknown"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			getenv := func(key string) string {
				if key == "LINEAR_COMMENTS_CACHE" {
					return filepath.Join(root, "cache.json")
				}
				return ""
			}
			if got := runWithArgs(t.Context(), getenv, test.args, &stdout, &stderr); got != 2 {
				t.Fatalf("run exit=%d stderr=%q", got, stderr.String())
			}
		})
	}
}

func TestRunRequiresIssuePrefix(t *testing.T) {
	root := t.TempDir()
	watchlistPath := filepath.Join(root, "watchlist.json")
	if err := os.WriteFile(watchlistPath, []byte(`{"schemaVersion":"watchlist-v1","issues":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(filepath.Join(root, "cache.json"), new(testCache(func(cache *Cache) { cache.FetchedAt = new(isoTimestamp(time.Now())) }))); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		env    string
		args   []string
		expect int
	}{
		{name: "missing flag and env", args: []string{"read", "--watchlist", watchlistPath}, expect: 2},
		{name: "empty flag", args: []string{"--issue-prefix", "", "read", "--watchlist", watchlistPath}, expect: 2},
		{name: "env fallback", env: "EX,DEMO", args: []string{"read", "--watchlist", watchlistPath}, expect: 0},
		{name: "flag", args: []string{"--issue-prefix", "EX", "--issue-prefix", "DEMO", "read", "--watchlist", watchlistPath}, expect: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			getenv := func(key string) string {
				switch key {
				case "LINEAR_COMMENTS_CACHE":
					return filepath.Join(root, "cache.json")
				case issuePrefixEnv:
					return test.env
				}
				return ""
			}
			got := runWithArgs(t.Context(), getenv, test.args, &stdout, &stderr)
			if got != test.expect || (test.expect == 2 && !strings.Contains(stderr.String(), "at least one team key")) {
				t.Fatalf("run exit=%d stderr=%q", got, stderr.String())
			}
		})
	}
}
func TestRenderReport(t *testing.T) {
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	contract := testContract(t)
	watermark := "2026-10-03T23:00:00.000Z"
	issue := Issue{State: "In Progress", Assignee: new("Worker"), Truncated: TruncatedNone, Comments: []Comment{
		{ID: "at", CreatedAt: watermark, Author: "At watermark", AuthorID: new("reviewer-id"), Body: "At watermark body"},
		{ID: "reviewer", CreatedAt: "2026-10-03T23:00:00.001Z", Author: "Pat Reviewer", AuthorID: new("reviewer-id"), Body: "Reviewer body"},
		{ID: "own-user", CreatedAt: "2026-10-03T23:00:00.002Z", Author: "Worker", AuthorID: new("own-id"), Body: "Worker body"},
	}}
	cache := testCache(func(cache *Cache) {
		cache.OwnAuthorIDs = []string{"own-id", "viewer-id"}
		cache.Issues = newOrderedMap[Issue]()
		cache.Issues.set("EX-1", issue)
		cache.Issues.set("EX-2", Issue{State: "In Progress", Assignee: new("Worker"), Comments: []Comment{{ID: "poller", CreatedAt: "2026-10-03T22:00:00.000Z", Author: "Poller", AuthorID: new("viewer-id"), Body: "Poller body"}}})
		cache.Watched = []string{"EX-1", "EX-2"}
	})
	entries := []WatchEntry{{ID: "EX-1", Watermark: &watermark}, {ID: "EX-2"}}
	report := renderReport(Watchlist{SchemaVersion: "watchlist-v1", Issues: entries}, &cache, now, contract)
	for _, expected := range []string{
		"EX-1 [In Progress, assignee Worker] 1 new",
		"2026-10-03T23:00:00.001Z Pat Reviewer:",
		"2026-10-03T23:00:00.002Z Worker [own]:",
		"EX-2 [In Progress, assignee Worker] no new comments",
		"2026-10-03T22:00:00.000Z Poller [own]:",
	} {
		if !strings.Contains(report, expected) {
			t.Errorf("report missing %q: %s", expected, report)
		}
	}
	if strings.Contains(report, "At watermark body") {
		t.Fatalf("comment at watermark was included: %s", report)
	}
	ownOnly := Issue{State: "Todo", Truncated: TruncatedNone, Comments: []Comment{{ID: "own", CreatedAt: "2026-10-03T23:01:00.000Z", Author: "Worker", AuthorID: new("own-id"), Body: "own body"}}}
	cache.Issues.set("EX-1", ownOnly)
	if got := renderReport(Watchlist{Issues: []WatchEntry{{ID: "EX-1", Watermark: &watermark}}}, &cache, now, contract); !strings.Contains(got, "no new comments") || !strings.Contains(got, "Worker [own]:") {
		t.Fatalf("own-only report = %s", got)
	}
}

func TestRenderReportCacheMissesAndTruncation(t *testing.T) {
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	contract := testContract(t)
	cache := testCache(func(cache *Cache) {
		cache.Watched = []string{"EX-2"}
		cache.Missing = []string{"EX-2"}
		cache.Issues = newOrderedMap[Issue]()
	})
	watchlist := Watchlist{Issues: []WatchEntry{{ID: "EX-2"}}}
	if got := renderReport(watchlist, &cache, now, contract); !strings.Contains(got, "not returned by Linear at the last sweep") {
		t.Fatalf("missing issue report = %s", got)
	}
	cache.Missing = []string{}
	if got := renderReport(watchlist, &cache, now, contract); !strings.Contains(got, "not in cache yet") {
		t.Fatalf("unwatched issue report = %s", got)
	}
	watermark := "2026-10-03T22:00:00.000Z"
	for _, test := range []struct {
		name      string
		truncated Truncated
		watermark *string
		want      string
		avoid     string
	}{
		{name: "older", truncated: TruncatedOlder, watermark: &watermark, want: "older comments not cached", avoid: "newer comments not cached"},
		{name: "newer", truncated: TruncatedNewer, want: "newer comments not cached", avoid: "older comments not cached"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cache.Issues.set("EX-2", Issue{State: "Todo", Truncated: test.truncated, Comments: []Comment{{ID: "comment", CreatedAt: "2026-10-03T23:00:00.000Z", Author: "Reviewer"}}})
			got := renderReport(Watchlist{Issues: []WatchEntry{{ID: "EX-2", Watermark: test.watermark}}}, &cache, now, contract)
			if !strings.Contains(got, test.want) || strings.Contains(got, test.avoid) {
				t.Fatalf("truncation report = %s", got)
			}
		})
	}
}
