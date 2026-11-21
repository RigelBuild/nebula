package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
)

const cacheSchemaVersion = "linear-comments-v1"

var errInvalidCache = errors.New("comment cache has an invalid shape")

var errCacheNotFound = errors.New("cache file not found")

func kindPointer(value ErrorKind) *ErrorKind { return new(value) }
func parseCache(raw json.RawMessage) (Cache, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return Cache{}, errInvalidCache
	}
	var cache Cache
	if err := json.Unmarshal(raw, &cache); err != nil {
		return Cache{}, errInvalidCache
	}
	if cache.SchemaVersion != cacheSchemaVersion || (cache.Status != "ok" && cache.Status != statusStale) ||
		!hasJSONFields(fields, "schemaVersion", "status", "fetchedAt", "attemptedAt", "error", "rateLimitRemaining", "complexity", "ownAuthorIds", "watched", "missing", "warnings", "issues") {
		return Cache{}, errInvalidCache
	}
	if cache.FetchedAt != nil && !validDate(*cache.FetchedAt) || !validDate(cache.AttemptedAt) ||
		!validNullableString(fields["error"]) || !validNullableNumber(fields["rateLimitRemaining"]) ||
		!validNullableNumber(fields["complexity"]) || !validStringArray(fields["ownAuthorIds"]) ||
		!validStringArray(fields["watched"]) || !validStringArray(fields["missing"]) ||
		!validStringArray(fields["warnings"]) || !validIssues(fields["issues"]) {
		return Cache{}, errInvalidCache
	}
	if rawKind, ok := fields["errorKind"]; ok && !bytes.Equal(rawKind, []byte("null")) {
		var kind ErrorKind
		if err := json.Unmarshal(rawKind, &kind); err != nil {
			return Cache{}, errInvalidCache
		}
		if _, ok := errorKinds[kind]; !ok {
			return Cache{}, errInvalidCache
		}
		cache.ErrorKind = new(kind)
	} else {
		cache.ErrorKind = nil
	}
	if rawCount, ok := fields["revokeFailures"]; ok {
		var count float64
		if err := json.Unmarshal(rawCount, &count); err != nil || !safeInteger(count) {
			return Cache{}, errInvalidCache
		}
		cache.RevokeFailures = int(count)
	} else {
		cache.RevokeFailures = 0
	}
	return cache, nil
}

func hasJSONFields(fields map[string]json.RawMessage, names ...string) bool {
	for _, name := range names {
		if _, ok := fields[name]; !ok {
			return false
		}
	}
	return true
}

func validDate(value string) bool {
	_, ok := parseTime(value)
	return ok
}

func validNullableString(value json.RawMessage) bool {
	if bytes.Equal(value, []byte("null")) {
		return true
	}
	var text string
	return json.Unmarshal(value, &text) == nil
}

func validNullableNumber(value json.RawMessage) bool {
	if bytes.Equal(value, []byte("null")) {
		return true
	}
	number, ok := nullableFloat(value)
	return ok && number != nil && !math.IsNaN(*number) && !math.IsInf(*number, 0)
}

func validStringArray(value json.RawMessage) bool {
	if len(value) == 0 || value[0] != '[' {
		return false
	}
	var values []string
	return json.Unmarshal(value, &values) == nil
}

func validString(value json.RawMessage) bool {
	var text *string
	return json.Unmarshal(value, &text) == nil && text != nil
}

func validIssues(value json.RawMessage) bool {
	var issues map[string]json.RawMessage
	if len(value) == 0 || value[0] != '{' || json.Unmarshal(value, &issues) != nil || issues == nil {
		return false
	}
	for _, rawIssue := range issues {
		var issueFields map[string]json.RawMessage
		var issue Issue
		if json.Unmarshal(rawIssue, &issueFields) != nil || issueFields == nil ||
			!hasJSONFields(issueFields, "state", "assignee", "truncated", "comments") || !validString(issueFields["state"]) ||
			json.Unmarshal(rawIssue, &issue) != nil ||
			(issue.Truncated != TruncatedNone && issue.Truncated != TruncatedOlder && issue.Truncated != TruncatedNewer) ||
			!validNullableString(issueFields["assignee"]) || !validComments(issueFields["comments"]) {
			return false
		}
	}
	return true
}

func validComments(value json.RawMessage) bool {
	if len(value) == 0 || value[0] != '[' {
		return false
	}
	var comments []json.RawMessage
	if json.Unmarshal(value, &comments) != nil {
		return false
	}
	for _, rawComment := range comments {
		var fields map[string]json.RawMessage
		var comment Comment
		if json.Unmarshal(rawComment, &fields) != nil || fields == nil ||
			!hasJSONFields(fields, "id", "createdAt", "editedAt", "author", "authorId", "body") ||
			json.Unmarshal(rawComment, &comment) != nil || !validString(fields["id"]) || !validString(fields["author"]) || !validString(fields["body"]) ||
			!validDate(comment.CreatedAt) || !validNullableDate(fields["editedAt"]) ||
			!validNullableString(fields["authorId"]) {
			return false
		}
	}
	return true
}

func validNullableDate(value json.RawMessage) bool {
	if bytes.Equal(value, []byte("null")) {
		return true
	}
	var text string
	return json.Unmarshal(value, &text) == nil && validDate(text)
}

func readCache(path string) (*Cache, error) {
	contents, err := os.ReadFile(path) //nolint:gosec // The path is supplied by the operator's environment.
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errCacheNotFound
	}
	if err != nil {
		return nil, err
	}
	cache, parseErr := parseCache(contents)
	if errors.Is(parseErr, errInvalidCache) {
		// Malformed cache files are deliberately ignored, matching the TypeScript reader.
		return nil, errInvalidCache
	}
	if parseErr != nil {
		return nil, parseErr
	}
	return &cache, nil
}

func cacheReadIsAbsent(err error) bool {
	return errors.Is(err, errCacheNotFound) || errors.Is(err, errInvalidCache)
}

func staleCache(previous *Cache, attemptedAt string, failure *SweepError, warnings []string) Cache {
	cache := Cache{
		SchemaVersion:      cacheSchemaVersion,
		Status:             "stale",
		FetchedAt:          nil,
		AttemptedAt:        attemptedAt,
		Error:              new(failure.Message),
		ErrorKind:          kindPointer(failure.Kind),
		RateLimitRemaining: failure.RateLimitRemaining,
		OwnAuthorIDs:       []string{},
		Watched:            []string{},
		Missing:            []string{},
		Warnings:           append([]string{}, warnings...),
		Issues:             newOrderedMap[Issue](),
	}
	if previous != nil {
		cache.FetchedAt = previous.FetchedAt
		cache.Complexity = previous.Complexity
		cache.OwnAuthorIDs = previous.OwnAuthorIDs
		cache.Watched = previous.Watched
		cache.Missing = previous.Missing
		cache.Issues = previous.Issues
		cache.RevokeFailures = previous.RevokeFailures
		if cache.RateLimitRemaining == nil {
			cache.RateLimitRemaining = previous.RateLimitRemaining
		}
	}
	return cache
}

func newSince(issue Issue, watermark *string) []Comment {
	comments := make([]Comment, 0, len(issue.Comments))
	watermarkTime := int64(math.MinInt64)
	if watermark != nil {
		watermarkTime, _ = parseTimestamp(*watermark)
	}
	for _, comment := range issue.Comments {
		createdAt, _ := parseTimestamp(comment.CreatedAt)
		if createdAt > watermarkTime {
			comments = append(comments, comment)
		}
	}
	return comments
}

func newsSince(issue Issue, watermark *string, ownAuthorIDs []string) []Comment {
	comments := newSince(issue, watermark)
	news := make([]Comment, 0, len(comments))
	for _, comment := range comments {
		isOwn := false
		if comment.AuthorID != nil {
			if slices.Contains(ownAuthorIDs, *comment.AuthorID) {
				isOwn = true
			}
		}
		if !isOwn {
			news = append(news, comment)
		}
	}
	return news
}

func writeJSONAtomic(path string, value any) (returnErr error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("creating parent directory: %w", err)
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return fmt.Errorf("creating temporary filename: %w", err)
	}
	temporary := path + ".tmp-" + hex.EncodeToString(random[:])
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // The temporary file is beside the operator-selected destination.
	if err != nil {
		return fmt.Errorf("creating temporary file: %w", err)
	}
	defer func() {
		if returnErr != nil {
			removeErr := os.Remove(temporary)
			if removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
				returnErr = errors.Join(returnErr, fmt.Errorf("removing temporary file: %w", removeErr))
			}
		}
	}()
	if err := writeJSONIndented(file, value); err != nil {
		closeErr := file.Close()
		return errors.Join(fmt.Errorf("writing temporary file: %w", err), closeErr)
	}
	if err := file.Chmod(0o600); err != nil {
		closeErr := file.Close()
		return errors.Join(fmt.Errorf("setting temporary file mode: %w", err), closeErr)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing temporary file: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("replacing destination: %w", err)
	}
	return nil
}

func successLine(cache Cache, requests int, contract Contract) string {
	return fmt.Sprintf("%s issues=%d requests=%d remaining=%s", contract.SuccessPrefix, len(cache.Watched), requests, displayFloat(cache.RateLimitRemaining))
}
