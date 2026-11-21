package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"time"
)

type ErrorKind string

const (
	ErrorNoCredential ErrorKind = "no-credential" //nolint:gosec // This is a public error code, not a credential.
	ErrorAuth         ErrorKind = "auth"
	ErrorNetwork      ErrorKind = "network"
	ErrorHTTP         ErrorKind = "http"
	ErrorRateLimited  ErrorKind = "ratelimited"
	ErrorGraphQL      ErrorKind = "graphql"
	ErrorTimeout      ErrorKind = "timeout"
	ErrorDeadline     ErrorKind = "deadline"
	ErrorBadShape     ErrorKind = "bad-shape"
	ErrorMissing      ErrorKind = "missing"
	statusStale                 = "stale"
)

var errorKinds = map[ErrorKind]struct{}{
	ErrorNoCredential: {}, ErrorAuth: {}, ErrorNetwork: {}, ErrorHTTP: {},
	ErrorRateLimited: {}, ErrorGraphQL: {}, ErrorTimeout: {}, ErrorDeadline: {},
	ErrorBadShape: {}, ErrorMissing: {},
}

type SweepError struct {
	Message            string
	Kind               ErrorKind
	RateLimitRemaining *float64
}

func (e *SweepError) Error() string { return e.Message }

type Comment struct {
	ID        string  `json:"id"`
	CreatedAt string  `json:"createdAt"`
	EditedAt  *string `json:"editedAt"`
	Author    string  `json:"author"`
	AuthorID  *string `json:"authorId"`
	Body      string  `json:"body"`
}

type Truncated string

const (
	TruncatedNone  Truncated = "none"
	TruncatedOlder Truncated = "older"
	TruncatedNewer Truncated = "newer"
)

type Issue struct {
	State     string    `json:"state"`
	Assignee  *string   `json:"assignee"`
	Truncated Truncated `json:"truncated"`
	Comments  []Comment `json:"comments"`
}

// orderedMap retains JavaScript's insertion order for JSON object keys.
type orderedMap[T any] struct {
	keys   []string
	values map[string]T
}

func newOrderedMap[T any]() orderedMap[T] {
	return orderedMap[T]{keys: []string{}, values: map[string]T{}}
}

func (m *orderedMap[T]) set(key string, value T) {
	if m.values == nil {
		m.values = map[string]T{}
		m.keys = []string{}
	}
	if _, exists := m.values[key]; !exists {
		m.keys = append(m.keys, key)
	}
	m.values[key] = value
}

func (m orderedMap[T]) get(key string) (T, bool) {
	value, ok := m.values[key]
	return value, ok
}

func (m orderedMap[T]) each(fn func(string, T) bool) {
	for _, key := range m.keys {
		value, ok := m.values[key]
		if ok && !fn(key, value) {
			return
		}
	}
}

func (m orderedMap[T]) MarshalJSON() ([]byte, error) {
	var buffer bytes.Buffer
	buffer.WriteByte('{')
	first := true
	for _, key := range m.keys {
		value, ok := m.values[key]
		if !ok {
			continue
		}
		if !first {
			buffer.WriteByte(',')
		}
		first = false
		encodedKey, err := jsonString(key)
		if err != nil {
			return nil, err
		}
		buffer.Write(encodedKey)
		buffer.WriteByte(':')
		encodedValue, err := jsonNoHTML(value)
		if err != nil {
			return nil, err
		}
		buffer.Write(encodedValue)
	}
	buffer.WriteByte('}')
	return buffer.Bytes(), nil
}

func (m *orderedMap[T]) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token != json.Delim('{') {
		return errors.New("expected JSON object")
	}
	*m = newOrderedMap[T]()
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return errors.New("expected JSON object key")
		}
		var value T
		if err := decoder.Decode(&value); err != nil {
			return err
		}
		m.set(key, value)
	}
	_, err = decoder.Token()
	return err
}

func jsonNoHTML(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

func jsonString(value string) ([]byte, error) {
	return jsonNoHTML(value)
}

type Cache struct {
	SchemaVersion      string            `json:"schemaVersion"`
	Status             string            `json:"status"`
	FetchedAt          *string           `json:"fetchedAt"`
	AttemptedAt        string            `json:"attemptedAt"`
	Error              *string           `json:"error"`
	ErrorKind          *ErrorKind        `json:"errorKind"`
	RateLimitRemaining *float64          `json:"rateLimitRemaining"`
	Complexity         *float64          `json:"complexity"`
	OwnAuthorIDs       []string          `json:"ownAuthorIds"`
	Watched            []string          `json:"watched"`
	Missing            []string          `json:"missing"`
	Warnings           []string          `json:"warnings"`
	RevokeFailures     int               `json:"revokeFailures"`
	Issues             orderedMap[Issue] `json:"issues"`
}

type WatchEntry struct {
	ID        string  `json:"id"`
	Watermark *string `json:"watermark"`
}

type Watchlist struct {
	SchemaVersion string       `json:"schemaVersion"`
	Issues        []WatchEntry `json:"issues"`
}

type AgentWatchlist struct {
	Codename  string
	Watchlist Watchlist
}

type WatchlistsRead struct {
	Agents   []AgentWatchlist
	IDs      []string
	Warnings []string
	Skipped  []string
	Present  []string
}

type NotifyState struct {
	SchemaVersion string                         `json:"schemaVersion"`
	Markers       orderedMap[orderedMap[string]] `json:"markers"`
	AlertedKind   *ErrorKind                     `json:"alertedKind"`
}

type News struct {
	ID    string
	Count int
	Key   string
}

type Contract struct {
	SuccessPrefix   string `json:"successPrefix"`
	StaleAfterMS    int64  `json:"staleAfterMs"`
	FallbackAfterMS int64  `json:"fallbackAfterMs"`
}

func parseTime(value string) (time.Time, bool) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return parsed, err == nil
}

func parseTimestamp(value string) (int64, bool) {
	parsed, ok := parseTime(value)
	if !ok {
		return 0, false
	}
	return parsed.UnixMilli(), true
}

func nullableFloat(value json.RawMessage) (*float64, bool) {
	if bytes.Equal(value, []byte("null")) || len(value) == 0 {
		return nil, len(value) != 0
	}
	var number float64
	if err := json.Unmarshal(value, &number); err != nil {
		return nil, false
	}
	return new(number), true
}

func safeInteger(value float64) bool {
	return value >= 0 && value <= 9007199254740991 && value == float64(int64(value))
}

func displayFloat(value *float64) string {
	if value == nil {
		return "unknown"
	}
	return strconv.FormatFloat(*value, 'f', -1, 64)
}

func writeJSONIndented(writer io.Writer, value any) error {
	encoded, err := jsonNoHTML(value)
	if err != nil {
		return err
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, encoded, "", "  "); err != nil {
		return err
	}
	if _, err := pretty.WriteTo(writer); err != nil {
		return err
	}
	_, err = io.WriteString(writer, "\n")
	return err
}
