package httpretry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestNewClient(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     int
		recover    bool
		retryAfter string
		attempts   int
		delays     []time.Duration
		wantError  bool
	}{
		{name: "503 recovers", status: 503, recover: true, attempts: 2, delays: []time.Duration{5 * time.Second}},
		{name: "429 clamps Retry-After", status: 429, recover: true, retryAfter: "999", attempts: 2, delays: []time.Duration{30 * time.Second}},
		{name: "500 clamps Retry-After", status: 500, recover: true, retryAfter: "999", attempts: 2, delays: []time.Duration{30 * time.Second}},
		{name: "404 stops", status: 404, attempts: 1},
		{name: "401 stops", status: 401, attempts: 1},
		{name: "501 stops", status: 501, attempts: 1},
		{name: "505 stops", status: 505, attempts: 1},
		{name: "503 exhausts", status: 503, attempts: 3, delays: []time.Duration{5 * time.Second, 10 * time.Second}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				if test.recover && calls > 1 {
					w.WriteHeader(http.StatusOK)
					return
				}
				w.Header().Set("Retry-After", test.retryAfter)
				w.WriteHeader(test.status)
			}))
			defer server.Close()
			var delays []time.Duration
			client := NewClient(server.Client())
			retry := client.Transport.(*methodTransport).retry.Client
			backoff := retry.Backoff
			// Record the real policy's delay but skip waiting in this unit test.
			retry.Backoff = func(minimum, maximum time.Duration, attempt int, response *http.Response) time.Duration {
				delays = append(delays, backoff(minimum, maximum, attempt, response))
				return 0
			}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Do(request)
			if response != nil {
				if err := response.Body.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if (err != nil) != test.wantError {
				t.Fatalf("error=%v, wantError=%t", err, test.wantError)
			}
			if test.wantError && !strings.Contains(err.Error(), "3 attempt(s)") {
				t.Fatalf("missing ceiling: %v", err)
			}
			if calls != test.attempts {
				t.Fatalf("calls=%d, want %d", calls, test.attempts)
			}
			if len(delays) != len(test.delays) {
				t.Fatalf("delays=%v, want %v", delays, test.delays)
			}
			for i := range delays {
				if delays[i] != test.delays[i] {
					t.Fatalf("delays=%v, want %v", delays, test.delays)
				}
			}
		})
	}
}

func TestNewClientCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	client := NewClient(server.Client())
	client.Transport.(*methodTransport).retry.Client.Backoff = func(_, _ time.Duration, _ int, _ *http.Response) time.Duration { cancel(); return time.Hour }
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if response != nil {
		if closeErr := response.Body.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}

func TestErrorHandlerCancellationWinsOverResponse(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "canceled", true: "deadline"}[deadline], func(t *testing.T) {
			var ctx context.Context
			var cancel context.CancelFunc
			want := context.Canceled
			if deadline {
				ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
				want = context.DeadlineExceeded
			} else {
				ctx, cancel = context.WithCancel(t.Context())
				cancel()
			}
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.com/", nil)
			if err != nil {
				t.Fatal(err)
			}
			response := &http.Response{Request: request, Body: http.NoBody, StatusCode: http.StatusServiceUnavailable}
			handler := NewClient(nil).Transport.(*methodTransport).retry.Client.ErrorHandler
			result, err := handler(response, errors.New("service unavailable"), 1)
			if result != nil {
				defer result.Body.Close()
			}
			if !errors.Is(err, want) {
				t.Fatalf("error=%v, want %v", err, want)
			}
		})
	}
}

type testRoundTripper func(*http.Request) (*http.Response, error)

func (roundTrip testRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestNewClientDoesNotReplayPostAfterLostResponse(t *testing.T) {
	calls := 0
	client := NewClient(&http.Client{Transport: testRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls++
		return nil, syscall.ECONNRESET
	})})
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://example.com/pulls?sv=2024&sig=secret", strings.NewReader("body"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Transport.RoundTrip(request)
	if response != nil {
		defer response.Body.Close()
	}
	if !errors.Is(err, syscall.ECONNRESET) || calls != 1 || strings.Contains(err.Error(), "sig=") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}

func TestNewClientDoesNotRetryPostOnServiceUnavailable(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client := NewClient(server.Client())
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/pulls?sig=secret", strings.NewReader("body"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if calls != 1 || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d calls=%d", response.StatusCode, calls)
	}
}

func TestNewClientCancellationDuringRoundTrip(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	client := NewClient(&http.Client{Transport: testRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls++
		cancel()
		return nil, syscall.ECONNRESET
	})})
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if response != nil {
		defer response.Body.Close()
	}
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}

func TestRetryTransportSanitizesExhaustedResponse(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client := NewClient(server.Client())
	client.Transport.(*methodTransport).retry.Client.Backoff = func(_, _ time.Duration, _ int, _ *http.Response) time.Duration { return 0 }
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/ratecard?sv=2024&sig=secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Transport.RoundTrip(request)
	if response != nil {
		defer response.Body.Close()
	}
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "sig=") ||
		!strings.Contains(err.Error(), "503") || calls != 3 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}
