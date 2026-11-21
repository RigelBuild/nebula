// Package garage talks to the Garage v2 admin API.
package garage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf16"
)

const (
	healthPath        = "/health"
	apiPath           = "/v2"
	readinessInterval = 2 * time.Second
	errorBodyLimit    = 200
	// DefaultReadyTimeout is the readiness deadline when the caller sets none.
	DefaultReadyTimeout = 120 * time.Second
)

// Deps holds the side effects the admin client needs. HTTP carries the admin token, so
// every request through it is sent by noRedirect, which never follows a redirect.
type Deps struct {
	HTTP  *http.Client
	Sleep func(ctx context.Context, d time.Duration) error
	Now   func() time.Time
	Log   func(line string)
}

// noRedirect returns a shallow copy of client that never follows a redirect. Go keeps
// Authorization on a same-host redirect to another port, so following one would resend
// the admin token. The 3xx then surfaces as an HTTP status. The copy leaves the
// caller's client unchanged.
func noRedirect(client *http.Client) *http.Client {
	inner := *client
	inner.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &inner
}

// TrimTrailingSlashes removes every trailing slash from an admin URL.
func TrimTrailingSlashes(adminURL string) string {
	return strings.TrimRight(adminURL, "/")
}

// ErrorBody replaces every non-empty token with [redacted], then truncates to 200 UTF-16 units with an ellipsis.
// Redaction runs before truncation, so a token that straddles the cut is still replaced in full.
func ErrorBody(body string, tokens ...string) string {
	redacted := body
	for _, token := range tokens {
		if token != "" {
			redacted = strings.ReplaceAll(redacted, token, "[redacted]")
		}
	}
	units := utf16.Encode([]rune(redacted))
	if len(units) <= errorBodyLimit {
		return redacted
	}
	return string(utf16.Decode(units[:errorBodyLimit])) + "…"
}

// Send issues one admin API request and returns the status and full body.
func Send(ctx context.Context, deps Deps, adminURL, endpoint, method, token string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("encode %s request: %w", endpoint, err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, TrimTrailingSlashes(adminURL)+apiPath+"/"+endpoint, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("build %s request: %w", endpoint, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := noRedirect(deps.HTTP).Do(req)
	if err != nil {
		return 0, nil, err
	}
	text, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return 0, nil, fmt.Errorf("read %s response: %w", endpoint, err)
	}
	return response.StatusCode, text, nil
}

// DecodeJSON parses a response body. An empty body is not valid JSON.
func DecodeJSON(endpoint string, text []byte) (any, error) {
	var value any
	if err := json.Unmarshal(text, &value); err != nil {
		return nil, fmt.Errorf("%s returned invalid JSON", endpoint)
	}
	return value, nil
}

// Record reports whether v is a JSON object.
func Record(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

// StringArray reports whether v is a JSON array of strings.
func StringArray(v any) ([]string, bool) {
	items, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// WaitForAdmin polls /health until it returns 200 or the timeout elapses.
func WaitForAdmin(ctx context.Context, adminURL string, timeout time.Duration, deps Deps) error {
	if timeout < 0 {
		return errors.New("readyTimeoutMs must be a non-negative finite number")
	}
	deadline := deps.Now().Add(timeout)
	for first := true; first || deps.Now().Before(deadline); first = false {
		if ready(ctx, deps, adminURL) {
			return nil
		}
		remaining := deadline.Sub(deps.Now())
		if remaining <= 0 {
			break
		}
		if err := deps.Sleep(ctx, min(readinessInterval, remaining)); err != nil {
			return err
		}
	}
	return fmt.Errorf("Garage admin API was not ready within %dms", timeout.Milliseconds()) //nolint:staticcheck // ST1005: user-facing message, text is fixed.
}

// ready reports a 200 from /health. Any transport or close failure means not ready yet.
func ready(ctx context.Context, deps Deps, adminURL string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, TrimTrailingSlashes(adminURL)+healthPath, nil)
	if err != nil {
		return false
	}
	response, err := noRedirect(deps.HTTP).Do(req)
	if err != nil {
		return false
	}
	closeErr := response.Body.Close()
	return response.StatusCode == http.StatusOK && closeErr == nil
}
