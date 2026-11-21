package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec" //nolint:depguard // The poller invokes cotal directly with fixed argv and no shell.
	"strings"
	"time"
)

type cotalRunner func(context.Context, string, []string, time.Duration) (int, string, error)

type pendingAgentNews struct {
	Codename string
	News     []News
}

func emptyNotifyState() NotifyState {
	return NotifyState{SchemaVersion: "linear-comments-notify-v1", Markers: newOrderedMap[orderedMap[string]]()}
}

func parseNotifyState(raw json.RawMessage) (NotifyState, bool) {
	var state NotifyState
	if err := json.Unmarshal(raw, &state); err != nil || state.SchemaVersion != "linear-comments-notify-v1" {
		return emptyNotifyState(), false
	}
	if state.Markers.values == nil {
		return emptyNotifyState(), false
	}
	if state.AlertedKind != nil {
		if _, ok := errorKinds[*state.AlertedKind]; !ok {
			return emptyNotifyState(), false
		}
	}
	state.Markers.each(func(_ string, markers orderedMap[string]) bool {
		markers.each(func(_ string, key string) bool {
			if !validDate(key) {
				state.SchemaVersion = ""
			}
			return true
		})
		return true
	})
	if state.SchemaVersion == "" {
		return emptyNotifyState(), false
	}
	return state, true
}

func readNotifyState(path string) NotifyState {
	contents, err := os.ReadFile(path) //nolint:gosec // The path is supplied by the operator's environment.
	if err != nil {
		return emptyNotifyState()
	}
	state, ok := parseNotifyState(contents)
	if !ok {
		return emptyNotifyState()
	}
	return state
}

func newestTimestamp(comments []Comment) *string {
	var newest *string
	var newestTime time.Time
	for index, comment := range comments {
		value, _ := parseTime(comment.CreatedAt)
		if index == 0 || value.After(newestTime) {
			newest = new(comment.CreatedAt)
			newestTime = value
		}
	}
	return newest
}

func dmArgs(name, text string) []string {
	return []string{"send", "--to", name, text, "--space", "main"}
}

func pendingNews(prefixes issuePrefixes, agents []AgentWatchlist, cache Cache, state NotifyState) []pendingAgentNews {
	pending := []pendingAgentNews{}
	if cache.Status != "ok" {
		return pending
	}
	for _, agent := range agents {
		agentNews := []News{}
		markers, _ := state.Markers.get(agent.Codename)
		for _, entry := range agent.Watchlist.Issues {
			issue, ok := cache.Issues.get(entry.ID)
			if !ok || !prefixes.hasKey(entry.ID) {
				continue
			}
			comments := newsSince(issue, entry.Watermark, cache.OwnAuthorIDs)
			key := newestTimestamp(comments)
			if issue.Truncated == TruncatedNewer {
				key = newestTimestamp(issue.Comments)
			}
			if key == nil {
				continue
			}
			marker, marked := markers.get(entry.ID)
			keyTime, _ := parseTimestamp(*key)
			markerTime, markerValid := parseTimestamp(marker)
			if marked && markerValid && keyTime <= markerTime {
				continue
			}
			if len(comments) > 0 || issue.Truncated == TruncatedNewer {
				agentNews = append(agentNews, News{ID: entry.ID, Count: len(comments), Key: *key})
			}
		}
		if len(agentNews) > 0 {
			pending = append(pending, pendingAgentNews{Codename: agent.Codename, News: agentNews})
		}
	}
	return pending
}

func wakeText(news []News) string {
	parts := make([]string, 0, len(news))
	for _, entry := range news {
		noun := "comments"
		if entry.Count == 1 {
			noun = "comment"
		}
		parts = append(parts, fmt.Sprintf("%s: %d new %s", entry.ID, entry.Count, noun))
	}
	return joinWithSemicolon(parts) + "; run linear-comments"
}

func joinWithSemicolon(values []string) string {
	return strings.Join(values, "; ")
}

func sendWakes(ctx context.Context, pending []pendingAgentNews, state NotifyState, run cotalRunner, deadline time.Time, now func() time.Time, workspacesRoot string) NotifyState {
	markers := state.Markers
	for _, item := range pending {
		timeLeft := deadline.Sub(now())
		if timeLeft <= 0 {
			break
		}
		code, _, err := run(ctx, workspacesRoot, dmArgs(item.Codename, wakeText(item.News)), min(15*time.Second, timeLeft))
		if err != nil || code != 0 {
			continue
		}
		agentMarkers, ok := markers.get(item.Codename)
		if !ok {
			agentMarkers = newOrderedMap[string]()
		}
		for _, entry := range item.News {
			agentMarkers.set(entry.ID, entry.Key)
		}
		markers.set(item.Codename, agentMarkers)
	}
	state.Markers = markers
	return state
}

func pruneMarkers(state NotifyState, agents []AgentWatchlist, skipped, present []string) NotifyState {
	byCodename := map[string]AgentWatchlist{}
	for _, agent := range agents {
		byCodename[agent.Codename] = agent
	}
	markers := newOrderedMap[orderedMap[string]]()
	state.Markers.each(func(codename string, issueMarkers orderedMap[string]) bool {
		if !contains(present, codename) {
			return true
		}
		if contains(skipped, codename) {
			markers.set(codename, issueMarkers)
			return true
		}
		agent, ok := byCodename[codename]
		if !ok {
			markers.set(codename, issueMarkers)
			return true
		}
		watched := map[string]struct{}{}
		for _, entry := range agent.Watchlist.Issues {
			watched[entry.ID] = struct{}{}
		}
		kept := newOrderedMap[string]()
		issueMarkers.each(func(id string, key string) bool {
			if _, ok := watched[id]; ok {
				kept.set(id, key)
			}
			return true
		})
		if len(kept.keys) > 0 {
			markers.set(codename, kept)
		}
		return true
	})
	state.Markers = markers
	return state
}

func reportFailure(ctx context.Context, previous *Cache, current Cache, state NotifyState, run cotalRunner, timeout time.Duration, workspacesRoot string) NotifyState {
	if current.Status == "ok" {
		state.AlertedKind = nil
		return state
	}
	if timeout <= 0 || previous == nil || previous.Status != statusStale || current.ErrorKind == nil ||
		(state.AlertedKind != nil && *current.ErrorKind == *state.AlertedKind) {
		return state
	}
	lastGood := "never"
	if current.FetchedAt != nil {
		lastGood = *current.FetchedAt
	}
	message := fmt.Sprintf("linear-comment-poller: 2 failed sweeps in a row (%s: %s); last good %s", *current.ErrorKind, errorString(current.Error), lastGood)
	code, _, err := run(ctx, workspacesRoot, dmArgs("foreman", message), timeout)
	if err == nil && code == 0 {
		state.AlertedKind = kindPointer(*current.ErrorKind)
	}
	return state
}

func updateNotifications(ctx context.Context, path string, prefixes issuePrefixes, agents []AgentWatchlist, skipped, present []string, previous *Cache, current Cache, run cotalRunner, deadline time.Time, now func() time.Time, workspacesRoot string) error {
	state := readNotifyState(path)
	if current.Status == "ok" {
		state = pruneMarkers(state, agents, skipped, present)
		state = sendWakes(ctx, pendingNews(prefixes, agents, current, state), state, run, deadline, now, workspacesRoot)
	}
	timeout := min(15*time.Second, max(0, deadline.Sub(now())))
	state = reportFailure(ctx, previous, current, state, run, timeout, workspacesRoot)
	return writeJSONAtomic(path, state)
}

func errorString(value *string) string {
	if value == nil {
		return "unknown error"
	}
	return *value
}

func runCotal(ctx context.Context, root string, args []string, timeout time.Duration) (int, string, error) {
	if timeout <= 0 {
		return -1, "", context.DeadlineExceeded
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(ctx, "cotal", args...) //nolint:gosec // The executable is fixed and arguments are passed without a shell.
	command.Dir = root
	command.Stdout = io.Discard
	var stderr strings.Builder
	command.Stderr = &stderr
	err := command.Run()
	if err == nil {
		return 0, stderr.String(), nil
	}
	if exitError, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitError.ExitCode(), stderr.String(), nil
	}
	return -1, stderr.String(), err
}
