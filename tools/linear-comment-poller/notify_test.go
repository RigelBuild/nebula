package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const notifyAt1 = "2026-10-03T22:25:53.156Z"
const notifyAt2 = "2026-10-03T22:26:53.156Z"
const notifyAt3 = "2026-10-03T22:27:53.156Z"

func notifyIssue(comments []Comment, truncated Truncated) Issue {
	return Issue{State: "Todo", Truncated: truncated, Comments: comments}
}
func notifyCache(issues map[string]Issue) Cache {
	result := Cache{SchemaVersion: cacheSchemaVersion, Status: "ok", FetchedAt: new(notifyAt3), AttemptedAt: notifyAt3, OwnAuthorIDs: []string{"own-user-id", "app-viewer-id"}, Watched: []string{}, Missing: []string{}, Warnings: []string{}, Issues: newOrderedMap[Issue]()}
	for id, issue := range issues {
		result.Issues.set(id, issue)
		result.Watched = append(result.Watched, id)
	}
	return result
}
func notifyComment(id, at, authorID string) Comment {
	return Comment{ID: id, CreatedAt: at, Author: id, AuthorID: new(authorID), Body: id + " body"}
}
func notifyAgent(name string, entries ...WatchEntry) AgentWatchlist {
	return AgentWatchlist{Codename: name, Watchlist: Watchlist{SchemaVersion: "watchlist-v1", Issues: entries}}
}

func TestPendingNewsAndWakeText(t *testing.T) {
	agent := notifyAgent("observability", WatchEntry{ID: "EX-1"})
	cache := notifyCache(map[string]Issue{"EX-1": notifyIssue([]Comment{notifyComment("reviewer", notifyAt1, "reviewer-id"), notifyComment("agent", notifyAt2, "own-user-id")}, TruncatedNone)})
	pending := pendingNews(examplePrefixes(t), []AgentWatchlist{agent}, cache, emptyNotifyState())
	if len(pending) != 1 || len(pending[0].News) != 1 || pending[0].News[0].Count != 1 || pending[0].News[0].Key != notifyAt1 {
		t.Fatalf("pending = %#v", pending)
	}
	ownOnly := notifyCache(map[string]Issue{"EX-1": notifyIssue([]Comment{notifyComment("agent", notifyAt1, "own-user-id"), notifyComment("app", notifyAt2, "app-viewer-id")}, TruncatedNone)})
	if len(pendingNews(examplePrefixes(t), []AgentWatchlist{agent}, ownOnly, emptyNotifyState())) != 0 {
		t.Fatal("own comments created a wake")
	}
	truncated := notifyCache(map[string]Issue{"EX-1": notifyIssue([]Comment{notifyComment("agent", notifyAt2, "own-user-id")}, TruncatedNewer)})
	news := pendingNews(examplePrefixes(t), []AgentWatchlist{agent}, truncated, emptyNotifyState())
	if len(news) != 1 || news[0].News[0].Count != 0 || news[0].News[0].Key != notifyAt2 {
		t.Fatalf("newer-truncated news = %#v", news)
	}
	truncated.Status = "stale"
	if len(pendingNews(examplePrefixes(t), []AgentWatchlist{agent}, truncated, emptyNotifyState())) != 0 {
		t.Fatal("stale cache created wake")
	}
	if got := wakeText([]News{{ID: "EX-1", Count: 2}, {ID: "EX-2", Count: 1}}); got != "EX-1: 2 new comments; EX-2: 1 new comment; run linear-comments" {
		t.Fatalf("wake text = %q", got)
	}
	if !reflect.DeepEqual(dmArgs("observability", "wake"), []string{"send", "--to", "observability", "wake", "--space", "main"}) {
		t.Fatal("dm arguments changed")
	}
}

func TestSendWakes(t *testing.T) {
	agents := []AgentWatchlist{notifyAgent("observability", WatchEntry{ID: "EX-1"}, WatchEntry{ID: "EX-2"}), notifyAgent("skills", WatchEntry{ID: "EX-2"})}
	cache := notifyCache(map[string]Issue{"EX-1": notifyIssue([]Comment{notifyComment("reviewer", notifyAt1, "reviewer-id")}, TruncatedNone), "EX-2": notifyIssue([]Comment{notifyComment("second", notifyAt3, "reviewer-id")}, TruncatedNone)})
	pending := pendingNews(examplePrefixes(t), agents, cache, emptyNotifyState())
	calls := [][]string{}
	updated := sendWakes(t.Context(), pending, emptyNotifyState(), func(_ context.Context, _ string, args []string, _ time.Duration) (int, string, error) {
		calls = append(calls, args)
		return 0, "", nil
	}, time.Date(2026, 10, 3, 22, 27, 53, 156000000, time.UTC).Add(10*time.Second), func() time.Time { return time.Date(2026, 10, 3, 22, 27, 53, 156000000, time.UTC) }, "/workspaces")
	if len(calls) != 2 || calls[0][2] != "observability" || calls[1][2] != "skills" {
		t.Fatalf("cotal calls = %#v", calls)
	}
	markers, _ := updated.Markers.get("observability")
	if got, ok := markers.get("EX-1"); !ok || got != notifyAt1 {
		t.Fatalf("markers = %#v", updated.Markers)
	}
	failed := sendWakes(t.Context(), pending, emptyNotifyState(), func(context.Context, string, []string, time.Duration) (int, string, error) { return 1, "offline", nil }, time.Now().Add(time.Minute), time.Now, "/workspaces")
	if len(failed.Markers.keys) != 0 {
		t.Fatal("failed wake marked")
	}
}

func TestPruneMarkers(t *testing.T) {
	state := emptyNotifyState()
	for _, name := range []string{"skipped", "parsed", "gone", "present"} {
		markers := newOrderedMap[string]()
		markers.set("EX-1", notifyAt1)
		markers.set("EX-2", notifyAt2)
		state.Markers.set(name, markers)
	}
	pruned := pruneMarkers(state, []AgentWatchlist{notifyAgent("parsed", WatchEntry{ID: "EX-1"})}, []string{"skipped"}, []string{"skipped", "parsed", "present"})
	if _, ok := pruned.Markers.get("gone"); ok {
		t.Fatal("removed agent marker retained")
	}
	parsed, _ := pruned.Markers.get("parsed")
	if _, ok := parsed.get("EX-2"); ok {
		t.Fatal("dropped issue marker retained")
	}
	for _, name := range []string{"skipped", "present"} {
		if _, ok := pruned.Markers.get(name); !ok {
			t.Fatalf("marker %s was dropped", name)
		}
	}
}

func TestReportFailure(t *testing.T) {
	previous := notifyCache(nil)
	previous.Status = "stale"
	previous.ErrorKind = kindPointer(ErrorHTTP)
	current := notifyCache(nil)
	current.Status = "stale"
	current.ErrorKind = kindPointer(ErrorHTTP)
	current.Error = new("Linear GraphQL returned HTTP 500")
	current.FetchedAt = nil
	calls := [][]string{}
	run := func(_ context.Context, _ string, args []string, _ time.Duration) (int, string, error) {
		calls = append(calls, args)
		return 0, "", nil
	}
	state := reportFailure(t.Context(), &previous, current, emptyNotifyState(), run, time.Second, "/workspaces")
	if len(calls) != 1 || calls[0][2] != "foreman" || !strings.Contains(calls[0][3], "2 failed sweeps in a row (http: Linear GraphQL returned HTTP 500); last good never") || state.AlertedKind == nil || *state.AlertedKind != ErrorHTTP {
		t.Fatalf("report = %#v calls=%#v", state, calls)
	}
	repeated := reportFailure(t.Context(), &previous, current, state, run, time.Second, "/workspaces")
	if len(calls) != 1 || repeated.AlertedKind == nil {
		t.Fatal("repeated alert was sent")
	}
	current.ErrorKind = kindPointer(ErrorAuth)
	reportFailure(t.Context(), &previous, current, state, func(context.Context, string, []string, time.Duration) (int, string, error) { return 1, "offline", nil }, time.Second, "/workspaces")
	if len(calls) != 1 {
		t.Fatal("failed report changed calls")
	}
	current.Status = "ok"
	cleared := reportFailure(t.Context(), &previous, current, state, run, time.Second, "/workspaces")
	if cleared.AlertedKind != nil {
		t.Fatal("successful sweep did not clear alert kind")
	}
	if got := reportFailure(t.Context(), &previous, current, emptyNotifyState(), run, 0, "/workspaces"); got.AlertedKind != nil {
		t.Fatal("deadline passed but report changed state")
	}
}

func TestSendWakesDeadlineAndMarkerAdvancement(t *testing.T) {
	first, second, third := notifyAgent("agent-one"), notifyAgent("agent-two"), notifyAgent("agent-three")
	firstNews := []pendingAgentNews{
		{Codename: first.Codename, News: []News{{ID: "EX-1", Count: 1, Key: notifyAt1}}},
		{Codename: second.Codename, News: []News{{ID: "EX-2", Count: 1, Key: notifyAt2}}},
		{Codename: third.Codename, News: []News{{ID: "EX-3", Count: 1, Key: notifyAt3}}},
	}
	now := time.Date(2026, 10, 3, 22, 27, 53, 156000000, time.UTC)
	deadline := now.Add(15 * time.Second)
	calls := [][]string{}
	state := sendWakes(t.Context(), firstNews, emptyNotifyState(), func(_ context.Context, _ string, args []string, timeout time.Duration) (int, string, error) {
		calls = append(calls, args)
		if timeout != 15*time.Second {
			t.Fatalf("timeout=%s", timeout)
		}
		now = deadline.Add(time.Nanosecond)
		return 0, "", nil
	}, deadline, func() time.Time { return now }, "/workspaces")
	if len(calls) != 1 || calls[0][2] != "agent-one" {
		t.Fatalf("deadline calls=%#v", calls)
	}
	if markers, ok := state.Markers.get("agent-one"); !ok || len(markers.keys) != 1 {
		t.Fatalf("first markers=%#v", state.Markers)
	}
	if _, ok := state.Markers.get("agent-two"); ok {
		t.Fatalf("second agent marked after deadline: %#v", state.Markers)
	}

	agent := notifyAgent("observability", WatchEntry{ID: "EX-1"})
	cache := notifyCache(map[string]Issue{"EX-1": notifyIssue([]Comment{notifyComment("reviewer", notifyAt1, "reviewer-id")}, TruncatedNone)})
	initial := emptyNotifyState()
	firstPending := pendingNews(examplePrefixes(t), []AgentWatchlist{agent}, cache, initial)
	markerNow := time.Date(2026, 10, 3, 22, 27, 53, 156000000, time.UTC)
	markerDeadline := markerNow.Add(time.Minute)
	marked := sendWakes(t.Context(), firstPending, initial, func(context.Context, string, []string, time.Duration) (int, string, error) { return 0, "", nil }, markerDeadline, func() time.Time { return markerNow }, "/workspaces")
	if next := pendingNews(examplePrefixes(t), []AgentWatchlist{agent}, cache, marked); len(next) != 0 {
		t.Fatalf("same comment repeated: %#v", next)
	}
	cache.Issues.set("EX-1", notifyIssue([]Comment{
		notifyComment("reviewer", notifyAt1, "reviewer-id"),
		notifyComment("reply", notifyAt2, "reviewer-id"),
	}, TruncatedNone))
	next := pendingNews(examplePrefixes(t), []AgentWatchlist{agent}, cache, marked)
	if len(next) != 1 || len(next[0].News) != 1 || next[0].News[0].Count != 2 || next[0].News[0].Key != notifyAt2 {
		t.Fatalf("newer comment pending=%#v", next)
	}
}

func TestPruneSkippedRecoveryPreservesMarker(t *testing.T) {
	state := emptyNotifyState()
	markers := newOrderedMap[string]()
	markers.set("EX-1", notifyAt1)
	state.Markers.set("skipped", markers)
	afterSkipped := pruneMarkers(state, nil, []string{"skipped"}, []string{"skipped"})
	recovered := notifyAgent("skipped", WatchEntry{ID: "EX-1"})
	afterRecovery := pruneMarkers(afterSkipped, []AgentWatchlist{recovered}, nil, []string{"skipped"})
	got, ok := afterRecovery.Markers.get("skipped")
	if !ok || len(got.keys) != 1 || len(pendingNews(examplePrefixes(t), []AgentWatchlist{recovered}, notifyCache(map[string]Issue{"EX-1": notifyIssue([]Comment{notifyComment("reviewer", notifyAt1, "reviewer-id")}, TruncatedNone)}), afterRecovery)) != 0 {
		t.Fatalf("skipped marker was not retained: %#v", afterRecovery.Markers)
	}
}

func TestUpdateNotificationsPersistsMarkers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notify.json")
	initial := emptyNotifyState()
	markers := newOrderedMap[string]()
	markers.set("EX-1", notifyAt1)
	initial.Markers.set("observability", markers)
	contents, err := json.Marshal(initial)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	current := notifyCache(map[string]Issue{"EX-1": notifyIssue([]Comment{notifyComment("reviewer", notifyAt1, "reviewer-id"), notifyComment("reply", notifyAt2, "reviewer-id")}, TruncatedNone)})
	calls := 0
	err = updateNotifications(t.Context(), path, examplePrefixes(t), []AgentWatchlist{notifyAgent("observability", WatchEntry{ID: "EX-1"})}, nil, []string{"observability"}, &current, current, func(context.Context, string, []string, time.Duration) (int, string, error) {
		calls++
		return 0, "", nil
	}, time.Date(2026, 10, 3, 22, 27, 53, 156000000, time.UTC), func() time.Time { return time.Date(2026, 10, 3, 22, 26, 53, 156000000, time.UTC) }, "/workspaces")
	if err != nil || calls != 1 {
		t.Fatalf("update error=%v calls=%d", err, calls)
	}
	updated := readNotifyState(path)
	updatedMarkers, _ := updated.Markers.get("observability")
	if key, ok := updatedMarkers.get("EX-1"); !ok || key != notifyAt2 {
		t.Fatalf("persisted markers = %#v", updated.Markers)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("notify mode=%v err=%v", info, err)
	}
	contents, err = os.ReadFile(path)
	want := "{\n" +
		"  \"schemaVersion\": \"linear-comments-notify-v1\",\n" +
		"  \"markers\": {\n" +
		"    \"observability\": {\n" +
		"      \"EX-1\": \"2026-10-03T22:26:53.156Z\"\n" +
		"    }\n" +
		"  },\n" +
		"  \"alertedKind\": null\n" +
		"}\n"
	if err != nil || string(contents) != want {
		t.Fatalf("notify bytes = %q, err=%v", contents, err)
	}
}
