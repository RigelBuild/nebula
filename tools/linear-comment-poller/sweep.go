package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/linearauth"
)

const (
	sweepDeadline      = 120 * time.Second
	revokeFailureLimit = 96
)

type SweepDeps struct {
	IssuePrefixes   issuePrefixes
	WorkspacesRoot  string
	CachePath       string
	NotifyPath      string
	Credentials     *linearauth.Credentials
	OwnUserID       string
	AgentsAppUserID string
	Client          *http.Client
	Now             func() time.Time
	RunCotal        cotalRunner
	Stdout          io.Writer
	Stderr          io.Writer
}

type sweepOutcome struct {
	cache    Cache
	requests int
	viewerID string
}

func runSweep(ctx context.Context, deps SweepDeps, logger *slog.Logger, contract Contract) int {
	started := deps.Now()
	deadline := started.Add(sweepDeadline)
	previous, err := readCache(deps.CachePath)
	if cacheReadIsAbsent(err) {
		previous, err = nil, nil
	}
	if err != nil {
		_, _ = fmt.Fprintf(deps.Stderr, "Unable to read comment cache: %v\n", err)
		return 1
	}
	revokeFailures := 0
	if previous != nil {
		revokeFailures = previous.RevokeFailures
	}
	watchlists, watchErr := readWatchlists(deps.IssuePrefixes, deps.WorkspacesRoot)
	if watchErr != nil {
		failure := &SweepError{Message: safeFailureMessage(watchErr), Kind: ErrorBadShape}
		return writeFailedSweep(ctx, deps, logger, failedSweepContext{previous: previous, deadline: deadline, failure: failure, revokeFailures: revokeFailures, warnings: []string{}})
	}
	if len(watchlists.IDs) == 0 {
		ownIDs := uniqueTrimmed(deps.OwnUserID, deps.AgentsAppUserID)
		if previous != nil {
			ownIDs = previous.OwnAuthorIDs
		}
		cache := Cache{
			SchemaVersion: cacheSchemaVersion, Status: "ok", FetchedAt: new(isoTimestamp(started)),
			AttemptedAt: isoTimestamp(started), RateLimitRemaining: previousFloat(previous, true),
			Complexity: previousFloat(previous, false), OwnAuthorIDs: ownIDs, Watched: []string{}, Missing: []string{},
			Warnings: watchlists.Warnings, RevokeFailures: revokeFailures, Issues: newOrderedMap[Issue](),
		}
		if err := writeJSONAtomic(deps.CachePath, cache); err != nil {
			logger.Error("writing successful cache", "error", err)
			return 1
		}
		if err := updateNotifications(ctx, deps.NotifyPath, deps.IssuePrefixes, watchlists.Agents, watchlists.Skipped, watchlists.Present, previous, cache, deps.RunCotal, deadline, deps.Now, deps.WorkspacesRoot); err != nil {
			logger.Error("updating notifications", "error", err)
			return 1
		}
		_, _ = fmt.Fprintln(deps.Stdout, successLine(cache, 0, contract))
		return 0
	}
	if !started.Before(deadline) {
		return writeFailedSweep(ctx, deps, logger, failedSweepContext{previous: previous, watchlists: &watchlists, deadline: deadline, failure: &SweepError{Message: deadlineMessage, Kind: ErrorDeadline}, revokeFailures: revokeFailures, warnings: watchlists.Warnings})
	}
	if deps.Credentials == nil || strings.TrimSpace(deps.OwnUserID) == "" {
		failure := &SweepError{Message: "Linear client credentials or LINEAR_OWN_USER_ID are missing", Kind: ErrorNoCredential}
		return writeFailedSweep(ctx, deps, logger, failedSweepContext{previous: previous, watchlists: &watchlists, deadline: deadline, failure: failure, revokeFailures: revokeFailures, warnings: watchlists.Warnings})
	}
	mintTimeLeft := deadline.Sub(deps.Now())
	if mintTimeLeft <= 0 {
		return writeFailedSweep(ctx, deps, logger, failedSweepContext{previous: previous, watchlists: &watchlists, deadline: deadline, failure: &SweepError{Message: deadlineMessage, Kind: ErrorDeadline}, revokeFailures: revokeFailures, warnings: watchlists.Warnings})
	}
	mintContext, cancelMint := context.WithTimeout(ctx, min(10*time.Second, mintTimeLeft))
	token, err := linearauth.MintAppToken(mintContext, deps.Client, *deps.Credentials, "read")
	cancelMint()
	if err != nil {
		failure := classifyMintError(err)
		return writeFailedSweep(ctx, deps, logger, failedSweepContext{previous: previous, watchlists: &watchlists, deadline: deadline, failure: failure, revokeFailures: revokeFailures, warnings: watchlists.Warnings})
	}
	outcome, failure := performSweep(ctx, deps, watchlists, token.AccessToken, deadline)
	revokeContext, cancelRevoke := context.WithTimeout(ctx, 10*time.Second)
	revokeErr := linearauth.RevokeAppToken(revokeContext, deps.Client, *deps.Credentials, token)
	cancelRevoke()
	warnings := append([]string{}, watchlists.Warnings...)
	if revokeErr != nil {
		revokeFailures++
		warnings = append(warnings, "OAuth token revocation failed")
	} else {
		revokeFailures = 0
	}
	if revokeFailures >= revokeFailureLimit {
		failure = &SweepError{Message: "OAuth token revocation failure limit reached", Kind: ErrorAuth, RateLimitRemaining: outcome.cache.RateLimitRemaining}
	}
	if failure == nil && !deps.Now().Before(deadline) {
		failure = &SweepError{Message: "Sweep deadline reached", Kind: ErrorDeadline, RateLimitRemaining: outcome.cache.RateLimitRemaining}
	}
	if failure != nil {
		return writeFailedSweep(ctx, deps, logger, failedSweepContext{previous: previous, watchlists: &watchlists, deadline: deadline, failure: failure, revokeFailures: revokeFailures, warnings: warnings})
	}
	if len(outcome.cache.Issues.keys) == 0 {
		failure = &SweepError{Message: fmt.Sprintf("Linear returned none of the %d watched issues", len(watchlists.IDs)), Kind: ErrorMissing, RateLimitRemaining: outcome.cache.RateLimitRemaining}
		return writeFailedSweep(ctx, deps, logger, failedSweepContext{previous: previous, watchlists: &watchlists, deadline: deadline, failure: failure, revokeFailures: revokeFailures, warnings: warnings})
	}
	ownIDs := uniqueTrimmed(deps.OwnUserID, deps.AgentsAppUserID, outcome.viewerID)
	cache := successfulCache(deps.Now(), watchlists.IDs, outcome.cache.Issues, warnings, ownIDs, outcome.cache.RateLimitRemaining, outcome.cache.Complexity, revokeFailures)
	if err := writeJSONAtomic(deps.CachePath, cache); err != nil {
		logger.Error("writing successful cache", "error", err)
		return 1
	}
	if err := updateNotifications(ctx, deps.NotifyPath, deps.IssuePrefixes, watchlists.Agents, watchlists.Skipped, watchlists.Present, previous, cache, deps.RunCotal, deadline, deps.Now, deps.WorkspacesRoot); err != nil {
		logger.Error("updating notifications", "error", err)
		return 1
	}
	if len(cache.Missing) > 0 {
		if _, err := fmt.Fprintf(deps.Stderr, "linear-comment-poller: watched issues not returned by Linear: %s\n", strings.Join(cache.Missing, ", ")); err != nil {
			logger.Error("writing missing issue warning", "error", err)
			return 1
		}
	}
	if _, err := fmt.Fprintln(deps.Stdout, successLine(cache, outcome.requests, contract)); err != nil {
		logger.Error("writing success line", "error", err)
		return 1
	}
	return 0
}

func performSweep(ctx context.Context, deps SweepDeps, watchlists WatchlistsRead, accessToken string, deadline time.Time) (sweepOutcome, *SweepError) {
	zeroComplexity := 0.0
	outcome := sweepOutcome{cache: Cache{Issues: newOrderedMap[Issue](), Complexity: &zeroComplexity}}
	viewerID := ""
	for start := 0; start < len(watchlists.IDs); start += chunkSize {
		if !deps.Now().Before(deadline) {
			return outcome, &SweepError{Message: deadlineMessage, Kind: ErrorDeadline, RateLimitRemaining: outcome.cache.RateLimitRemaining}
		}
		end := min(start+chunkSize, len(watchlists.IDs))
		numbers := make([]int64, 0, end-start)
		for _, id := range watchlists.IDs[start:end] {
			value, _ := strconv.ParseInt(id[strings.LastIndexByte(id, '-')+1:], 10, 64)
			numbers = append(numbers, value)
		}
		outcome.requests++
		result, err := fetchChunk(ctx, deps.IssuePrefixes, numbers, accessToken, deps.Client, min(30*time.Second, deadline.Sub(deps.Now())))
		if err != nil {
			if sweepError, ok := errors.AsType[*SweepError](err); ok {
				sweepError.RateLimitRemaining = chooseFloat(sweepError.RateLimitRemaining, outcome.cache.RateLimitRemaining)
				return outcome, sweepError
			}
			return outcome, &SweepError{Message: "Linear sweep failed", Kind: ErrorBadShape, RateLimitRemaining: outcome.cache.RateLimitRemaining}
		}
		outcome.cache.RateLimitRemaining = result.RateLimitRemaining
		outcome.cache.Complexity = addFloat(outcome.cache.Complexity, result.Complexity)
		if viewerID != "" && viewerID != result.ViewerID {
			return outcome, &SweepError{Message: "Linear viewer changed between chunks", Kind: ErrorBadShape, RateLimitRemaining: result.RateLimitRemaining}
		}
		viewerID = result.ViewerID
		result.Issues.each(func(id string, issue Issue) bool {
			outcome.cache.Issues.set(id, issue)
			return true
		})
		if !deps.Now().Before(deadline) {
			return outcome, &SweepError{Message: deadlineMessage, Kind: ErrorDeadline, RateLimitRemaining: outcome.cache.RateLimitRemaining}
		}
	}
	outcome.viewerID = viewerID
	return outcome, nil
}

func successfulCache(now time.Time, watched []string, issues orderedMap[Issue], warnings, ownIDs []string, remaining, complexity *float64, revokeFailures int) Cache {
	missing := make([]string, 0)
	for _, id := range watched {
		if _, ok := issues.get(id); !ok {
			missing = append(missing, id)
		}
	}
	attempted := isoTimestamp(now)
	return Cache{SchemaVersion: cacheSchemaVersion, Status: "ok", FetchedAt: new(attempted), AttemptedAt: attempted,
		OwnAuthorIDs: append([]string{}, ownIDs...), Watched: append([]string{}, watched...), Missing: missing,
		Warnings: append([]string{}, warnings...), RevokeFailures: revokeFailures, Issues: issues,
		RateLimitRemaining: remaining, Complexity: complexity}
}
func isoTimestamp(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

type failedSweepContext struct {
	previous       *Cache
	watchlists     *WatchlistsRead
	deadline       time.Time
	failure        *SweepError
	revokeFailures int
	warnings       []string
}

func writeFailedSweep(ctx context.Context, deps SweepDeps, logger *slog.Logger, failure failedSweepContext) int {
	cache := staleCache(failure.previous, isoTimestamp(deps.Now()), failure.failure, failure.warnings)
	cache.RevokeFailures = failure.revokeFailures
	if err := writeJSONAtomic(deps.CachePath, cache); err != nil {
		logger.Error("writing stale cache", "error", err)
		return 1
	}
	var agents []AgentWatchlist
	var skipped, present []string
	if failure.watchlists != nil {
		agents, skipped, present = failure.watchlists.Agents, failure.watchlists.Skipped, failure.watchlists.Present
	}
	if err := updateNotifications(ctx, deps.NotifyPath, deps.IssuePrefixes, agents, skipped, present, failure.previous, cache, deps.RunCotal, failure.deadline, deps.Now, deps.WorkspacesRoot); err != nil {
		logger.Error("updating notifications", "error", err)
		return 1
	}
	if _, err := fmt.Fprintln(deps.Stderr, errorString(cache.Error)); err != nil {
		logger.Error("writing failure line", "error", err)
	}
	return 1
}

func classifyMintError(err error) *SweepError {
	status := statusFromMintError(err.Error())
	if status != 0 {
		kind := ErrorHTTP
		if status >= 400 && status < 500 {
			kind = ErrorAuth
		}
		return &SweepError{Message: fmt.Sprintf("Linear token request failed: HTTP %d", status), Kind: kind}
	}
	if isTimeoutError(err) {
		return &SweepError{Message: "Linear token request timed out", Kind: ErrorTimeout}
	}
	var shapeError *json.UnmarshalTypeError
	if errors.As(err, &shapeError) || strings.Contains(err.Error(), "no access_token") || strings.Contains(err.Error(), "invalid character") || strings.Contains(err.Error(), "cannot unmarshal") {
		return &SweepError{Message: "Linear token request failed: invalid token response", Kind: ErrorBadShape}
	}
	return &SweepError{Message: "Linear token request failed: network error (" + errorCause(err) + ")", Kind: ErrorNetwork}
}

func statusFromMintError(message string) int {
	const marker = "client_credentials token request failed: "
	_, remainder, found := strings.Cut(message, marker)
	if !found || len(remainder) < 3 {
		return 0
	}
	status, err := strconv.Atoi(remainder[:3])
	if err != nil || status < 100 || status > 599 || (len(remainder) > 3 && remainder[3] != ' ') {
		return 0
	}
	return status
}

func safeFailureMessage(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "request timed out"
	}
	return "Linear sweep failed"
}

func previousFloat(previous *Cache, rate bool) *float64 {
	if previous == nil {
		return nil
	}
	if rate {
		return previous.RateLimitRemaining
	}
	return previous.Complexity
}

func chooseFloat(first, second *float64) *float64 {
	if first != nil {
		return first
	}
	return second
}

func addFloat(first, second *float64) *float64 {
	if first == nil && second == nil {
		return nil
	}
	value := 0.0
	if first != nil {
		value += *first
	}
	if second != nil {
		value += *second
	}
	return new(value)
}

func uniqueTrimmed(values ...string) []string {
	result := []string{}
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
