// SPDX-License-Identifier: MIT OR Apache-2.0

// Package transientretry retries an operation whose failure a classifier marks
// transient, with bounded exponential backoff.
package transientretry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Class names the retry bucket a failure falls into.
type Class string

// ExternalHTTPTransient is the retry class for transient external HTTP and
// network failures.
const ExternalHTTPTransient Class = "external-http-transient"

// Bounds caps the attempts and the backoff between them. Allow, when set, is
// asked before each retry with the error and the delay about to be slept. A
// false answer stops the retry and returns the error. A nil Allow allows every
// retry the other bounds permit. The publisher uses it to enforce a shared
// wall-clock ceiling.
type Bounds struct {
	Attempts int
	BaseWait time.Duration
	MaxWait  time.Duration
	Allow    func(err error, delay time.Duration) bool
}

// DefaultBounds matches the TS DEFAULT_TRANSIENT_RETRY_BOUNDS: three attempts,
// 10s base, 60s cap.
var DefaultBounds = Bounds{Attempts: 3, BaseWait: 10 * time.Second, MaxWait: 60 * time.Second}

// HTTPStatusError carries an HTTP status and Retry-After header from a failed
// response. The external-HTTP classifier reads its status and headers.
type HTTPStatusError struct {
	Status     int
	Header     http.Header
	StatusText string
	Err        error
}

func (e *HTTPStatusError) Error() string {
	if e == nil {
		return "HTTP status error <nil>"
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("HTTP %d %s", e.Status, e.StatusText)
}

// Unwrap exposes the wrapped error. A nil receiver unwraps to nil.
func (e *HTTPStatusError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// ClassifyExternalHTTP reports whether err is a transient external HTTP or
// network failure. An HTTP status takes precedence over transport text.
func ClassifyExternalHTTP(err error) Class {
	if err == nil {
		return ""
	}
	if status, ok := statusOf(err); ok {
		if status == http.StatusTooManyRequests ||
			(status >= 500 && status <= 599 && status != http.StatusNotImplemented && status != http.StatusHTTPVersionNotSupported) {
			return ExternalHTTPTransient
		}
		return ""
	}
	if transientCode(err) {
		return ExternalHTTPTransient
	}
	return ""
}

func statusOf(err error) (int, bool) {
	var status *HTTPStatusError
	if errors.As(err, &status) && status != nil && status.Status != 0 {
		return status.Status, true
	}
	return 0, false
}

func transientCode(err error) bool {
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ETIMEDOUT) {
		return true
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return dns.IsTimeout || dns.IsTemporary
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Timeout() {
		return true
	}
	text := err.Error()
	return strings.Contains(text, "connection reset by peer") ||
		strings.Contains(text, "temporary failure in name resolution") ||
		strings.Contains(text, "EAI_AGAIN")
}

// retryAfter reads a Retry-After header from err and returns its wait, capped
// at maxWait. It reports false when the header is absent or unreadable.
func retryAfter(err error, now time.Time, maxWait time.Duration) (time.Duration, bool) {
	var status *HTTPStatusError
	if !errors.As(err, &status) || status == nil || status.Header == nil {
		return 0, false
	}
	value := strings.TrimSpace(status.Header.Get("Retry-After"))
	if value == "" {
		return 0, false
	}
	if seconds, parseErr := strconv.ParseFloat(value, 64); parseErr == nil && seconds >= 0 {
		// Clamp in seconds first: a huge value overflows time.Duration.
		if seconds >= maxWait.Seconds() {
			return maxWait, true
		}
		return time.Duration(seconds * float64(time.Second)), true
	}
	if at, parseErr := http.ParseTime(value); parseErr == nil {
		return min(max(0, at.Sub(now)), maxWait), true
	}
	return 0, false
}

// Do runs op up to bounds.Attempts times. It retries only errors that classify
// returns a non-empty Class for, and it rethrows the final operation error.
func Do(ctx context.Context, op func(context.Context) error, classify func(error) Class, bounds Bounds, sleep Sleeper) error {
	return doWithClock(ctx, op, classify, bounds, time.Now, sleep)
}

// doWithClock is Do with an injectable clock. A Retry-After header on the
// failure sets the delay before the exponential backoff, capped at MaxWait.
func doWithClock(ctx context.Context, op func(context.Context) error, classify func(error) Class, bounds Bounds, clock func() time.Time, sleep Sleeper) error {
	for attempt := 1; ; attempt++ {
		err := op(ctx)
		if err == nil {
			return nil
		}
		class := classify(err)
		delay, ok := retryAfter(err, clock(), bounds.MaxWait)
		if !ok {
			delay = min(bounds.BaseWait*time.Duration(1<<(attempt-1)), bounds.MaxWait)
		}
		if class == "" || attempt >= bounds.Attempts || (bounds.Allow != nil && !bounds.Allow(err, delay)) {
			return err
		}
		slog.Default().Info("retrying transient failure",
			"class", string(class),
			"attempt", attempt+1,
			"attempts", bounds.Attempts,
			"delay", delay.String(),
			"error", err.Error())
		if err := sleep(ctx, delay); err != nil {
			return err
		}
	}
}

// Sleeper waits for d or until ctx is done. Tests inject one that records d.
type Sleeper func(ctx context.Context, d time.Duration) error

// SleepCtx waits d, or returns ctx's error first.
func SleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
