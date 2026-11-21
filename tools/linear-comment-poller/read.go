package main

import (
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"strings"
	"time"
)

const fallbackLine = "fallback: you may run one list_comments per watched issue per hour until the cache is fresh"

type freshnessResult struct {
	Stale    bool
	Fallback bool
	Lines    []string
}

func freshness(cache *Cache, now time.Time, contract Contract) freshnessResult {
	var age *int64
	var stale bool
	if cache != nil && cache.FetchedAt != nil {
		fetchedAt, ok := parseTimestamp(*cache.FetchedAt)
		if ok {
			value := now.UnixMilli() - fetchedAt
			age = &value
			stale = cache.Status == statusStale || value < 0 || value > contract.StaleAfterMS
		} else {
			stale = true
		}
	} else {
		stale = true
	}
	fallback := stale && (age == nil || *age > contract.FallbackAfterMS)
	if !stale && cache != nil && cache.FetchedAt != nil && age != nil {
		fetchedAt, _ := parseTime(*cache.FetchedAt)
		return freshnessResult{
			Lines: []string{fmt.Sprintf("cache: fresh — fetched %s (%s)", formatTime(fetchedAt), ageText(*age))},
		}
	}
	reason := "no cache"
	if cache != nil {
		reason = errorLabel(*cache)
	}
	lines := []string{fmt.Sprintf("cache: STALE — %s; %s; newer replies may be missing", reason, lastGoodText(cache, age))}
	if fallback {
		lines = append(lines, fallbackLine)
	}
	return freshnessResult{Stale: true, Fallback: fallback, Lines: lines}
}

func formatTime(value time.Time) string { return value.UTC().Format("2006-01-02 15:04 UTC") }

func ageText(milliseconds int64) string {
	return fmt.Sprintf("%d min ago", int64(math.Floor(float64(milliseconds)/60000)))
}

func lastGoodText(cache *Cache, age *int64) string {
	if cache == nil || cache.FetchedAt == nil {
		return "last good never"
	}
	fetchedAt, ok := parseTime(*cache.FetchedAt)
	if !ok {
		return "last good never"
	}
	ageValue := int64(0)
	if age != nil {
		ageValue = *age
	}
	return fmt.Sprintf("last good %s (%s)", formatTime(fetchedAt), ageText(ageValue))
}

func errorLabel(cache Cache) string {
	if cache.ErrorKind != nil && *cache.ErrorKind == ErrorRateLimited {
		if cache.RateLimitRemaining == nil {
			return "RATELIMITED"
		}
		return fmt.Sprintf("RATELIMITED (remaining %s)", displayFloat(cache.RateLimitRemaining))
	}
	if cache.ErrorKind == nil {
		if cache.Error != nil {
			return *cache.Error
		}
		return "unknown error"
	}
	kind := strings.ToUpper(string(*cache.ErrorKind))
	if cache.Error != nil && *cache.Error != "" {
		return fmt.Sprintf("%s (%s)", kind, *cache.Error)
	}
	return kind
}

func renderReport(watchlist Watchlist, cache *Cache, now time.Time, contract Contract) string {
	current := freshness(cache, now, contract)
	lines := append([]string{}, current.Lines...)
	for _, entry := range watchlist.Issues {
		var issue Issue
		var exists bool
		if cache != nil {
			issue, exists = cache.Issues.get(entry.ID)
		}
		if !exists {
			if cache != nil && contains(cache.Missing, entry.ID) {
				lines = append(lines, entry.ID+" not returned by Linear at the last sweep (deleted, moved, or the app lost access); no comments are watched")
			} else {
				lines = append(lines, entry.ID+" not in cache yet (added after the last sweep, or not found in Linear)")
			}
			continue
		}
		comments := newSince(issue, entry.Watermark)
		newsCount := 0
		if cache != nil {
			newsCount = len(newsSince(issue, entry.Watermark, cache.OwnAuthorIDs))
		}
		assignee := "unassigned"
		if issue.Assignee != nil {
			assignee = *issue.Assignee
		}
		status := "no new comments"
		if newsCount > 0 {
			status = fmt.Sprintf("%d new", newsCount)
		}
		lines = append(lines, fmt.Sprintf("%s [%s, assignee %s] %s", entry.ID, issue.State, assignee, status))
		for _, comment := range comments {
			own := cache != nil && comment.AuthorID != nil && contains(cache.OwnAuthorIDs, *comment.AuthorID)
			author := comment.Author
			if own {
				author += " [own]"
			}
			lines = append(lines, fmt.Sprintf("  %s %s:", comment.CreatedAt, author), "  "+comment.Body)
		}
		if issue.Truncated == TruncatedNewer {
			lines = append(lines, "  newer comments not cached — run list_comments once for "+entry.ID)
		} else if olderCommentsMissing(issue, entry.Watermark) {
			lines = append(lines, "  older comments not cached — run list_comments once for "+entry.ID)
		}
	}
	return strings.Join(lines, "\n")
}

func olderCommentsMissing(issue Issue, watermark *string) bool {
	if issue.Truncated != TruncatedOlder {
		return false
	}
	if watermark == nil {
		return true
	}
	if len(issue.Comments) == 0 {
		return false
	}
	watermarkAt, watermarkOK := parseTimestamp(*watermark)
	oldestAt, oldestOK := parseTimestamp(issue.Comments[0].CreatedAt)
	return watermarkOK && oldestOK && watermarkAt < oldestAt
}

func runRead(argv []string, cachePath string, now time.Time, stdout, stderr io.Writer, contract Contract, prefixes issuePrefixes) int {
	if len(argv) != 2 || argv[0] != "--watchlist" || argv[1] == "" {
		_, _ = fmt.Fprintln(stderr, "usage: linear-comments --watchlist <path>")
		return 2
	}
	contents, err := os.ReadFile(argv[1])
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	watchlist, err := parseWatchlist(prefixes, contents)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	cache, err := readCache(cachePath)
	if err != nil {
		if !cacheReadIsAbsent(err) {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		cache = nil
	}
	if _, err := io.WriteString(stdout, renderReport(watchlist, cache, now, contract)+"\n"); err != nil {
		return 1
	}
	if freshness(cache, now, contract).Stale {
		return 1
	}
	return 0
}

func contains(values []string, value string) bool {
	return slices.Contains(values, value)
}
