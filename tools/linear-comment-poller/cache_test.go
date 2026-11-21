package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testTime = "2026-10-03T22:25:53.156Z"

func testIssue() Issue {
	return Issue{State: "Todo", Truncated: TruncatedNone, Comments: []Comment{
		{ID: "own-user", CreatedAt: testTime, Author: "Worker", AuthorID: new("own-user-id"), Body: "own"},
		{ID: "app", CreatedAt: "2026-10-03T22:26:53.156Z", Author: "Poller", AuthorID: new("app-id"), Body: "app"},
		{ID: "reviewer", CreatedAt: "2026-10-03T22:27:53.156Z", Author: "Reviewer", AuthorID: new("reviewer-id"), Body: "new"},
	}}
}

func testCache(overrides func(*Cache)) Cache {
	cache := Cache{
		SchemaVersion: cacheSchemaVersion, Status: "ok", FetchedAt: new(testTime), AttemptedAt: testTime,
		RateLimitRemaining: new(float64(4999)), Complexity: new(float64(13)), OwnAuthorIDs: []string{"own-user-id", "app-id"},
		Watched: []string{"EX-1"}, Missing: []string{}, Warnings: []string{}, Issues: newOrderedMap[Issue](),
	}
	cache.Issues.set("EX-1", testIssue())
	if overrides != nil {
		overrides(&cache)
	}
	return cache
}

func testContract(t *testing.T) Contract {
	t.Helper()
	contract, err := loadContract()
	if err != nil {
		t.Fatal(err)
	}
	return contract
}

func TestParseCache(t *testing.T) {
	t.Run("parses caches and defaults old revoke count", func(t *testing.T) {
		cache := testCache(nil)
		encoded, err := json.Marshal(cache)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatal(err)
		}
		delete(fields, "revokeFailures")
		old, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parseCache(old)
		if err != nil || parsed.RevokeFailures != 0 || strings.Join(parsed.OwnAuthorIDs, ",") != "own-user-id,app-id" {
			t.Fatalf("parse old cache = %#v, %v", parsed, err)
		}
	})
	t.Run("rejects invalid revoke counts", func(t *testing.T) {
		for _, item := range []struct{ name, value string }{{"negative", "-1"}, {"fraction", "1.5"}, {"string", `"1"`}} {
			t.Run(item.name, func(t *testing.T) {
				cache := testCache(nil)
				encoded, err := json.Marshal(cache)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(encoded, &fields); err != nil {
					t.Fatal(err)
				}
				fields["revokeFailures"] = json.RawMessage(item.value)
				bad, err := json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := parseCache(bad); err == nil {
					t.Fatal("parseCache accepted invalid revoke count")
				}
			})
		}
	})
	t.Run("accepts empty issue state and comment text fields", func(t *testing.T) {
		cache := testCache(nil)
		issue, _ := cache.Issues.get("EX-1")
		issue.State = ""
		issue.Comments[0].ID = ""
		issue.Comments[0].Author = ""
		issue.Comments[0].AuthorID = new("")
		issue.Comments[0].Body = ""
		encoded, err := json.Marshal(cache)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parseCache(encoded); err != nil {
			t.Fatalf("parseCache rejected empty strings: %v", err)
		}
	})
}

func TestCacheWatermarkFiltering(t *testing.T) {
	issue := testIssue()
	if got := commentIDs(newSince(issue, new(testTime))); strings.Join(got, ",") != "app,reviewer" {
		t.Fatalf("newSince = %v", got)
	}
	if got := commentIDs(newsSince(issue, new(testTime), []string{"own-user-id", "app-id"})); strings.Join(got, ",") != "reviewer" {
		t.Fatalf("newsSince = %v", got)
	}
	if got := commentIDs(newsSince(issue, nil, []string{"own-user-id", "app-id"})); strings.Join(got, ",") != "reviewer" {
		t.Fatalf("newsSince nil = %v", got)
	}
}

func TestCacheStalePreservesData(t *testing.T) {
	previous := testCache(nil)
	previous.RevokeFailures = 5
	stale := staleCache(&previous, "2026-10-04T00:00:00.000Z", &SweepError{Message: "bad response", Kind: ErrorHTTP}, []string{})
	if stale.Status != "stale" || *stale.FetchedAt != testTime || stale.RevokeFailures != 5 {
		t.Fatalf("stale cache = %#v", stale)
	}
	if _, ok := stale.Issues.get("EX-1"); !ok {
		t.Fatal("previous issue was lost")
	}
}

func TestCacheWriteModeAndBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "cache.json")
	if err := writeJSONAtomic(path, testCache(nil)); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(contents), "\n") || !strings.HasPrefix(string(contents), "{\n  \"schemaVersion\": \"linear-comments-v1\",\n  \"status\": \"ok\"") {
		t.Fatalf("unexpected JSON bytes: %q", contents[:min(len(contents), 100)])
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v err=%v", info, err)
	}
	parsed, err := readCache(path)
	if err != nil || parsed == nil {
		t.Fatalf("read TS-compatible cache = %v, %v", parsed, err)
	}
}

func TestSuccessLine(t *testing.T) {
	line := successLine(testCache(nil), 2, testContract(t))
	if line != "linear-comment-poller: sweep ok issues=1 requests=2 remaining=4999" {
		t.Fatalf("success line = %q", line)
	}
}

func commentIDs(comments []Comment) []string {
	ids := make([]string, 0, len(comments))
	for _, comment := range comments {
		ids = append(ids, comment.ID)
	}
	return ids
}

func TestReadExistingCache(t *testing.T) {
	cache := testCache(nil)
	encoded, err := json.Marshal(cache)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "cache.json")
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	parsed, err := readCache(path)
	if err != nil || parsed == nil || parsed.SchemaVersion != cacheSchemaVersion {
		t.Fatalf("read existing cache = %#v, %v", parsed, err)
	}
}

func TestEmbeddedContract(t *testing.T) {
	contract, err := loadContract()
	if err != nil {
		t.Fatal(err)
	}
	if contract.SuccessPrefix == "" || contract.StaleAfterMS <= 0 || contract.FallbackAfterMS <= contract.StaleAfterMS {
		t.Fatalf("invalid embedded contract: %#v", contract)
	}
}

func TestFreshnessAndRendering(t *testing.T) {
	contract := testContract(t)
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	fresh := testCache(func(cache *Cache) { cache.FetchedAt = new("2026-10-03T23:31:00.000Z") })
	if freshness(&fresh, now, contract).Stale {
		t.Fatal("fresh cache marked stale")
	}
	stale := testCache(func(cache *Cache) { cache.FetchedAt = new("2026-10-03T23:29:00.000Z") })
	if !freshness(&stale, now, contract).Stale {
		t.Fatal("old cache marked fresh")
	}
	missing := freshness(nil, now, contract)
	if !missing.Fallback || !strings.Contains(strings.Join(missing.Lines, "\n"), "last good never") {
		t.Fatalf("missing cache freshness = %#v", missing)
	}
	cache := testCache(nil)
	text := renderReport(Watchlist{SchemaVersion: "watchlist-v1", Issues: []WatchEntry{{ID: "EX-1", Watermark: new(testTime)}}}, &cache, now, contract)
	if !strings.Contains(text, "EX-1 [Todo, assignee unassigned] 1 new") || !strings.Contains(text, "Reviewer:") || strings.Contains(text, "own body") {
		t.Fatalf("unexpected report: %s", text)
	}
}

func TestCacheJSONByteContract(t *testing.T) {
	cache := Cache{
		SchemaVersion: cacheSchemaVersion,
		Status:        "ok",
		AttemptedAt:   testTime,
		OwnAuthorIDs:  []string{},
		Watched:       []string{},
		Missing:       []string{},
		Warnings:      []string{},
		Issues:        newOrderedMap[Issue](),
	}
	path := filepath.Join(t.TempDir(), "cache.json")
	if err := writeJSONAtomic(path, cache); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n" +
		"  \"schemaVersion\": \"linear-comments-v1\",\n" +
		"  \"status\": \"ok\",\n" +
		"  \"fetchedAt\": null,\n" +
		"  \"attemptedAt\": \"2026-10-03T22:25:53.156Z\",\n" +
		"  \"error\": null,\n" +
		"  \"errorKind\": null,\n" +
		"  \"rateLimitRemaining\": null,\n" +
		"  \"complexity\": null,\n" +
		"  \"ownAuthorIds\": [],\n" +
		"  \"watched\": [],\n" +
		"  \"missing\": [],\n" +
		"  \"warnings\": [],\n" +
		"  \"revokeFailures\": 0,\n" +
		"  \"issues\": {}\n" +
		"}\n"
	if string(got) != want {
		t.Fatalf("serialized cache bytes = %q, want %q", got, want)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("cache mode = %v err=%v", info, err)
	}
}
