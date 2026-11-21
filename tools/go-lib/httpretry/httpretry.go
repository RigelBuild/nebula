// SPDX-License-Identifier: MIT OR Apache-2.0

// Package httpretry configures bounded retries for external HTTP services.
package httpretry

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/hashicorp/go-retryablehttp"
)

type requestMethodKey struct{}

type methodTransport struct {
	retry *retryablehttp.RoundTripper
}

func (transport *methodTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	ctx := context.WithValue(request.Context(), requestMethodKey{}, request.Method)
	return transport.retry.RoundTrip(request.WithContext(ctx))
}

// NewClient preserves the supplied HTTP client's transport, timeout and redirect
// policy. Only GET and HEAD requests are retried. Payload read/decode failures
// are outside the retry boundary.
// Callers with secret query parameters must sanitize the url.Error from http.Client.Do.
func NewClient(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	retry := retryablehttp.NewClient()
	inner := *client
	inner.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	retry.HTTPClient = &inner
	retry.Logger = nil
	retry.RetryMax = 2 // Two retries plus the initial request: three attempts.
	retry.RetryWaitMin = 5 * time.Second
	retry.RetryWaitMax = 30 * time.Second
	retry.CheckRetry = func(ctx context.Context, response *http.Response, err error) (bool, error) {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		method, _ := ctx.Value(requestMethodKey{}).(string)
		if method != http.MethodGet && method != http.MethodHead {
			return false, nil
		}
		if err != nil {
			if dns, ok := errors.AsType[*net.DNSError](err); ok {
				return dns.IsTimeout || dns.IsTemporary, nil
			}
			var network net.Error
			return (errors.As(err, &network) && network.Timeout()) || errors.Is(err, syscall.ECONNRESET), nil
		}
		if response.StatusCode == http.StatusTooManyRequests ||
			(response.StatusCode >= 500 && response.StatusCode < 600 &&
				response.StatusCode != http.StatusNotImplemented && response.StatusCode != http.StatusHTTPVersionNotSupported) {
			return true, fmt.Errorf("HTTP %s", response.Status)
		}
		return false, nil
	}
	retry.ErrorHandler = handleError
	retry.Backoff = func(minimum, maximum time.Duration, attempt int, response *http.Response) time.Duration {
		// DefaultBackoff honors Retry-After on 429 and 503. Apply the same handling
		// to all retryable statuses, then clamp even server-supplied delays.
		if response != nil && response.Header.Get("Retry-After") != "" {
			copyResponse := *response
			copyResponse.StatusCode = http.StatusTooManyRequests
			response = &copyResponse
		}
		return min(maximum, max(minimum, retryablehttp.DefaultBackoff(minimum, maximum, attempt, response)))
	}
	outer := *client
	outer.Transport = &methodTransport{retry: &retryablehttp.RoundTripper{Client: retry}}
	return &outer
}

func handleError(response *http.Response, err error, attempts int) (*http.Response, error) {
	if response != nil {
		if ctxErr := response.Request.Context().Err(); ctxErr != nil {
			if closeErr := response.Body.Close(); closeErr != nil {
				return nil, errors.Join(ctxErr, closeErr)
			}
			return nil, ctxErr
		}
	}
	if errors.Is(err, context.Canceled) {
		return nil, context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, context.DeadlineExceeded
	}
	if response != nil {
		closeErr := response.Body.Close()
		if closeErr != nil {
			return nil, fmt.Errorf("close exhausted HTTP response after %d attempt(s): %w", attempts, closeErr)
		}
		request := response.Request
		return nil, fmt.Errorf("%s %s%s: HTTP %d after %d attempt(s)", request.Method, request.URL.Host, request.URL.EscapedPath(), response.StatusCode, attempts)
	}
	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		target, parseErr := url.Parse(urlErr.URL)
		if parseErr == nil {
			return nil, fmt.Errorf("%s %s%s: HTTP request failed after %d attempt(s): %w",
				strings.ToUpper(urlErr.Op), target.Host, target.EscapedPath(), attempts, urlErr.Err)
		}
	}
	return nil, fmt.Errorf("HTTP request failed after %d attempt(s): %w", attempts, err)
}
