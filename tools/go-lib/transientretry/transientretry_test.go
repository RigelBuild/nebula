package transientretry

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, time.October, 6, 0, 0, 0, 0, time.UTC)

func fixedClock() time.Time { return fixedNow }

// recordingSleep records each requested delay and never waits.
func recordingSleep(sleeps *[]time.Duration) Sleeper {
	return func(_ context.Context, d time.Duration) error {
		*sleeps = append(*sleeps, d)
		return nil
	}
}

var errTransient = errors.New("transient")

func classifyTransient(err error) Class {
	if errors.Is(err, errTransient) {
		return "transient"
	}
	return ""
}

func TestDoStopsOnNonTransient(t *testing.T) {
	calls := 0
	want := errors.New("permanent")
	var sleeps []time.Duration
	err := Do(t.Context(), func(context.Context) error {
		calls++
		return want
	}, classifyTransient, DefaultBounds, recordingSleep(&sleeps))
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
	if len(sleeps) != 0 {
		t.Errorf("sleeps = %v, want none", sleeps)
	}
}

func TestDoRetriesWithoutAllowWhileOtherBoundsPermit(t *testing.T) {
	calls := 0
	var sleeps []time.Duration
	err := Do(t.Context(), func(context.Context) error {
		calls++
		if calls < 3 {
			return errTransient
		}
		return nil
	}, classifyTransient, Bounds{Attempts: 3, BaseWait: time.Second, MaxWait: time.Minute}, recordingSleep(&sleeps))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
	if len(sleeps) != 2 || sleeps[0] != time.Second || sleeps[1] != 2*time.Second {
		t.Errorf("sleeps = %v, want [1s 2s]", sleeps)
	}
}

func TestDoAllowFalseStopsBeforeSleep(t *testing.T) {
	calls := 0
	var sleeps []time.Duration
	bounds := Bounds{
		Attempts: 3,
		BaseWait: time.Second,
		MaxWait:  time.Minute,
		Allow:    func(error, time.Duration) bool { return false },
	}
	err := Do(t.Context(), func(context.Context) error {
		calls++
		return errTransient
	}, classifyTransient, bounds, recordingSleep(&sleeps))
	if !errors.Is(err, errTransient) {
		t.Fatalf("err = %v, want %v", err, errTransient)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
	if len(sleeps) != 0 {
		t.Errorf("sleeps = %v, want none", sleeps)
	}
}

func TestDoAllowTrueRetriesToCompletion(t *testing.T) {
	calls := 0
	var sleeps []time.Duration
	var asked []time.Duration
	bounds := Bounds{
		Attempts: 3,
		BaseWait: time.Second,
		MaxWait:  time.Minute,
		Allow: func(_ error, delay time.Duration) bool {
			asked = append(asked, delay)
			return true
		},
	}
	err := Do(t.Context(), func(context.Context) error {
		calls++
		if calls < 3 {
			return errTransient
		}
		return nil
	}, classifyTransient, bounds, recordingSleep(&sleeps))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if len(asked) != 2 || len(sleeps) != 2 {
		t.Errorf("asked = %v, sleeps = %v, want two each", asked, sleeps)
	}
}

func TestDoBackoffIsBoundedAndExhausts(t *testing.T) {
	calls := 0
	var sleeps []time.Duration
	bounds := Bounds{Attempts: 4, BaseWait: time.Second, MaxWait: 3 * time.Second}
	err := Do(t.Context(), func(context.Context) error {
		calls++
		return errTransient
	}, classifyTransient, bounds, recordingSleep(&sleeps))
	if !errors.Is(err, errTransient) {
		t.Fatalf("err = %v, want %v", err, errTransient)
	}
	if calls != 4 {
		t.Errorf("calls = %d, want 4", calls)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 3 * time.Second}
	if len(sleeps) != len(want) {
		t.Fatalf("sleeps = %v, want %v", sleeps, want)
	}
	for i := range want {
		if sleeps[i] != want[i] {
			t.Errorf("sleep %d = %v, want %v", i, sleeps[i], want[i])
		}
	}
}

func TestSleepCtxReturnsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := SleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestSleepCtxWaitsForTimer(t *testing.T) {
	if err := SleepCtx(t.Context(), time.Millisecond); err != nil {
		t.Fatalf("SleepCtx: %v", err)
	}
}

func TestClassifyExternalHTTP(t *testing.T) {
	t.Run("retries only temporary HTTP and network failures", func(t *testing.T) {
		for _, status := range []int{429, 500, 503, 599} {
			err := &HTTPStatusError{Status: status}
			if got := ClassifyExternalHTTP(err); got != ExternalHTTPTransient {
				t.Errorf("status %d: got %q, want %q", status, got, ExternalHTTPTransient)
			}
		}
		for _, status := range []int{400, 401, 403, 404, 422, 501, 505} {
			err := &HTTPStatusError{Status: status, Err: syscall.ECONNRESET}
			if got := ClassifyExternalHTTP(err); got != "" {
				t.Errorf("status %d with transport error: got %q, want empty", status, got)
			}
		}
		if got := ClassifyExternalHTTP(syscall.ECONNRESET); got != ExternalHTTPTransient {
			t.Errorf("ECONNRESET: got %q", got)
		}
		if got := ClassifyExternalHTTP(syscall.ETIMEDOUT); got != ExternalHTTPTransient {
			t.Errorf("ETIMEDOUT: got %q", got)
		}
		timeoutDNS := &net.DNSError{Err: "timeout", Name: "example.test", IsTimeout: true}
		if got := ClassifyExternalHTTP(timeoutDNS); got != ExternalHTTPTransient {
			t.Errorf("DNS timeout: got %q", got)
		}
		if got := ClassifyExternalHTTP(errors.New("invalid payload")); got != "" {
			t.Errorf("payload error: got %q, want empty", got)
		}
	})
}

func TestDoHonorsClampedRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
		want   time.Duration
	}{
		{name: "seconds clamped to the 60s cap", header: "120", want: 60 * time.Second},
		{name: "HTTP date 25s ahead", header: "Thu, 06 Oct 2026 00:00:25 GMT", want: 25 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sleeps []time.Duration
			calls := 0
			op := func(context.Context) error {
				calls++
				if calls == 1 {
					h := http.Header{}
					h.Set("Retry-After", tc.header)
					return &HTTPStatusError{Status: http.StatusTooManyRequests, Header: h}
				}
				return nil
			}
			if err := doWithClock(t.Context(), op, ClassifyExternalHTTP, DefaultBounds, fixedClock, recordingSleep(&sleeps)); err != nil {
				t.Fatalf("doWithClock: %v", err)
			}
			if len(sleeps) != 1 || sleeps[0] != tc.want {
				t.Errorf("sleeps = %v, want [%v]", sleeps, tc.want)
			}
		})
	}
}

func TestDoWithClockStopsOnNonTransient(t *testing.T) {
	calls := 0
	want := &HTTPStatusError{Status: http.StatusNotFound}
	var sleeps []time.Duration
	err := doWithClock(t.Context(), func(context.Context) error {
		calls++
		return want
	}, ClassifyExternalHTTP, DefaultBounds, fixedClock, recordingSleep(&sleeps))
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
	if len(sleeps) != 0 {
		t.Errorf("sleeps = %v, want none", sleeps)
	}
}

func TestRetryAfterCapsHugeValues(t *testing.T) {
	for _, value := range []string{"1e12", "1e300", "+Inf"} {
		t.Run(value, func(t *testing.T) {
			h := http.Header{}
			h.Set("Retry-After", value)
			err := &HTTPStatusError{Status: http.StatusTooManyRequests, Header: h}
			got, ok := retryAfter(err, fixedNow, time.Minute)
			if !ok || got != time.Minute {
				t.Errorf("retryAfter(%q) = %v, %v; want 1m0s, true", value, got, ok)
			}
		})
	}
}

func TestTypedNilHTTPStatusErrorIsAbsent(t *testing.T) {
	var nilStatus *HTTPStatusError
	bare := error(nilStatus)
	wrapped := fmt.Errorf("wrap: %w", bare)
	for name, err := range map[string]error{"bare": bare, "wrapped": wrapped} {
		t.Run(name, func(t *testing.T) {
			if status, ok := statusOf(err); ok {
				t.Errorf("statusOf = %d, true; want absent", status)
			}
			if _, ok := retryAfter(err, fixedNow, time.Minute); ok {
				t.Error("retryAfter reported a header for a nil status")
			}
			if got := ClassifyExternalHTTP(err); got != "" {
				t.Errorf("ClassifyExternalHTTP = %q; want empty", got)
			}
			calls := 0
			var sleeps []time.Duration
			gotErr := Do(t.Context(), func(context.Context) error {
				calls++
				return err
			}, ClassifyExternalHTTP, DefaultBounds, recordingSleep(&sleeps))
			if calls != 1 || len(sleeps) != 0 {
				t.Errorf("calls = %d, sleeps = %v; want one call, no sleep", calls, sleeps)
			}
			if gotErr == nil {
				t.Error("Do returned nil for a failing op")
			}
		})
	}
}
