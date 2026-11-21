package cdgate

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/transientretry"
)

// scripted returns each queued result in order and counts the calls. Running
// past the script fails the test instead of looping.
type scripted struct {
	t       *testing.T
	results []StepResult
	calls   int
}

func (s *scripted) exec(context.Context, bool) (StepResult, error) {
	s.calls++
	if len(s.results) == 0 {
		s.t.Fatalf("executor ran %d times, past its script", s.calls)
	}
	r := s.results[0]
	s.results = s.results[1:]
	return r, nil
}

// noSleep records each backoff without waiting, so the test does not wait on the wall clock.
func noSleep(waits *[]time.Duration) Sleeper {
	return func(_ context.Context, d time.Duration) error {
		*waits = append(*waits, d)
		return nil
	}
}

const transientOutput = "HTTP 429 Too Many Requests"

func TestDeployNonPulumiTargetRunsOnceAndPropagatesExit(t *testing.T) {
	s := &scripted{t: t, results: []StepResult{{Code: 3, Output: transientOutput}}}
	var waits []time.Duration
	code, err := Deploy(context.Background(), "nix:deploy-fleet", map[string]bool{"aws": true}, s.exec, noSleep(&waits))
	if err != nil || code != 3 {
		t.Fatalf("Deploy = %d, %v; want 3, nil", code, err)
	}
	if s.calls != 1 || len(waits) != 0 {
		t.Fatalf("calls = %d, waits = %v; a non-pulumi target must not retry", s.calls, waits)
	}
}

func TestDeployRetriesPulumiUpTransientBeforeResourceStep(t *testing.T) {
	s := &scripted{t: t, results: []StepResult{
		{Code: 1, Output: transientOutput},
		{Code: 0, Output: "ok"},
	}}
	var waits []time.Duration
	code, err := Deploy(context.Background(), "aws:up", map[string]bool{"aws": true}, s.exec, noSleep(&waits))
	if err != nil || code != 0 {
		t.Fatalf("Deploy = %d, %v; want 0, nil after the retry", code, err)
	}
	if s.calls != 2 {
		t.Fatalf("calls = %d, want 2 (one retry)", s.calls)
	}
}

func TestDeployPersistentTransientStopsAfterThreeAttempts(t *testing.T) {
	s := &scripted{t: t, results: []StepResult{
		{Code: 7, Output: transientOutput},
		{Code: 7, Output: transientOutput},
		{Code: 7, Output: transientOutput},
	}}
	var waits []time.Duration
	code, err := Deploy(context.Background(), "aws:up", map[string]bool{"aws": true}, s.exec, noSleep(&waits))
	if err != nil || code != 7 {
		t.Fatalf("Deploy = %d, %v; want the step's exit code 7 after three attempts", code, err)
	}
	if s.calls != transientretry.DefaultBounds.Attempts {
		t.Fatalf("calls = %d, want %d", s.calls, transientretry.DefaultBounds.Attempts)
	}
}

func TestDeployNeverRetriesAfterResourceStepStarted(t *testing.T) {
	s := &scripted{t: t, results: []StepResult{
		{Code: 1, Output: "+ aws:s3:Bucket bucket creating\n" + transientOutput},
	}}
	var waits []time.Duration
	code, err := Deploy(context.Background(), "aws:up", map[string]bool{"aws": true}, s.exec, noSleep(&waits))
	if err != nil || code != 1 {
		t.Fatalf("Deploy = %d, %v; want exit 1 with no retry", code, err)
	}
	if s.calls != 1 {
		t.Fatalf("calls = %d, want 1: a resource step started", s.calls)
	}
}

func TestDeployNeverRetriesOverflowedCapture(t *testing.T) {
	s := &scripted{t: t, results: []StepResult{
		{Code: 1, Output: transientOutput, Overflow: true},
	}}
	var waits []time.Duration
	if _, err := Deploy(context.Background(), "aws:up", map[string]bool{"aws": true}, s.exec, noSleep(&waits)); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if s.calls != 1 {
		t.Fatalf("calls = %d, want 1: an overflowed capture cannot prove no step ran", s.calls)
	}
}

func TestDeployNeverRetriesNonTransientFailure(t *testing.T) {
	s := &scripted{t: t, results: []StepResult{
		{Code: 2, Output: "error: configuration is invalid"},
	}}
	var waits []time.Duration
	code, err := Deploy(context.Background(), "aws:up", map[string]bool{"aws": true}, s.exec, noSleep(&waits))
	if err != nil || code != 2 {
		t.Fatalf("Deploy = %d, %v; want exit 2", code, err)
	}
	if s.calls != 1 {
		t.Fatalf("calls = %d, want 1", s.calls)
	}
}

func TestDeployDoesNotRetryExecutorErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "deadline exceeded", err: context.DeadlineExceeded},
		{name: "broken pipe", err: syscall.EPIPE},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			var waits []time.Duration
			exec := func(context.Context, bool) (StepResult, error) {
				calls++
				return StepResult{}, tc.err
			}
			code, err := Deploy(context.Background(), "aws:up", map[string]bool{"aws": true}, exec, noSleep(&waits))
			if !errors.Is(err, tc.err) || code != 0 {
				t.Fatalf("Deploy = %d, %v; want original executor error", code, err)
			}
			if calls != 1 || len(waits) != 0 {
				t.Fatalf("executor calls = %d, waits = %v; want one attempt and no retry", calls, waits)
			}
		})
	}
}

func TestDeployStopDuringBackoffStartsNoAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &scripted{t: t, results: []StepResult{{Code: 1, Output: transientOutput}}}
	sleep := func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}
	code, err := Deploy(ctx, "aws:up", map[string]bool{"aws": true}, s.exec, sleep)
	if err != nil || code != 1 {
		t.Fatalf("Deploy = %d, %v; want exit 1 with no error for a stopped deploy", code, err)
	}
	if s.calls != 1 {
		t.Fatalf("calls = %d, want 1: a stop during backoff must not start another attempt", s.calls)
	}
}
