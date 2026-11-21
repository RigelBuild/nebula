package cdgate

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestRunCDRetriedPulumiUpHoldsOneLeaseAcrossAttempts: a throttled pulumi up is
// retried inside one held Lease. The step acquires once before the first
// attempt and releases once after the last, so a retry never re-acquires.
func TestRunCDRetriedPulumiUpHoldsOneLeaseAcrossAttempts(t *testing.T) {
	api := &memLease{record: &LeaseRecord{ResourceVersion: "1"}}
	s := &scripted{t: t, results: []StepResult{
		{Code: 1, Output: transientOutput},
		{Code: 0, Output: "ok"},
	}}
	r, _ := runFixture(t, Verdict{Deploy: true}, s.exec)
	r.Lease = lease(api)
	if code := RunCD(context.Background(), "aws:up", r); code != 0 {
		t.Fatalf("RunCD = %d, want 0 after the retry succeeds", code)
	}
	if s.calls != 2 {
		t.Fatalf("attempts = %d, want 2", s.calls)
	}
	if got := holderWrites(api.writes); got < 1 {
		t.Fatalf("holder writes = %d, want the acquire", got)
	}
	// Exactly one release, and it is the last write.
	releases := 0
	for _, w := range api.writes {
		if w.HolderIdentity == "" {
			releases++
		}
	}
	if releases != 1 {
		t.Fatalf("releases = %d, want exactly 1 across both attempts: %+v", releases, api.writes)
	}
	if last := api.writes[len(api.writes)-1]; last.HolderIdentity != "" {
		t.Fatalf("final write = %+v, want the release last", last)
	}
}

// TestRunCDStopDuringRetryBackoffReleasesWithoutAttempt: a stop that arrives
// while a retry waits out its backoff starts no further attempt, and the Lease is
// still released. The sleep hook cancels the context on the backoff wait.
func TestRunCDStopDuringRetryBackoffReleasesWithoutAttempt(t *testing.T) {
	api := &memLease{record: &LeaseRecord{ResourceVersion: "1"}}
	s := &scripted{t: t, results: []StepResult{{Code: 1, Output: transientOutput}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, _ := runFixture(t, Verdict{Deploy: true}, nil)
	var attempted atomic.Bool
	r.Exec = func(ctx context.Context, capture bool) (StepResult, error) {
		attempted.Store(true)
		return s.exec(ctx, capture)
	}
	r.Lease = lease(api)
	r.Sleep = func(context.Context, time.Duration) error {
		// Cancel only after an attempt has started: that sleep is the retry backoff.
		if attempted.Load() {
			cancel()
			return context.Canceled
		}
		return nil
	}
	if code := RunCD(ctx, "aws:up", r); code != 1 {
		t.Fatalf("RunCD = %d, want 1 when a stop interrupts the retry", code)
	}
	if s.calls != 1 {
		t.Fatalf("attempts = %d, want exactly 1: a stop must not start another", s.calls)
	}
	last := api.writes[len(api.writes)-1]
	if last.HolderIdentity != "" {
		t.Fatalf("final Lease write = %+v, want the release after the stop", last)
	}
}

// retryRenewalLease signals when a successful renewal lands after the first
// attempt has finished. The initial holder write is the acquire.
type retryRenewalLease struct {
	*memLease
	firstAttemptDone         atomic.Bool
	renewedAfterFirstAttempt chan struct{}
}

func (l *retryRenewalLease) Update(ctx context.Context, w LeaseWrite) (*LeaseRecord, error) {
	renewalAfterFirstAttempt := l.firstAttemptDone.Load()
	record, err := l.memLease.Update(ctx, w)
	if err == nil && w.HolderIdentity != "" && renewalAfterFirstAttempt {
		select {
		case l.renewedAfterFirstAttempt <- struct{}{}:
		default:
		}
	}
	return record, err
}

// TestRunCDRetryKeepsRenewingAcrossAttempts: renewal runs for the whole retried
// deploy, not only the first attempt. The second attempt waits for a renewal
// observed after the first attempt has finished.
func TestRunCDRetryKeepsRenewingAcrossAttempts(t *testing.T) {
	api := &retryRenewalLease{
		memLease:                 &memLease{record: &LeaseRecord{ResourceVersion: "1"}},
		renewedAfterFirstAttempt: make(chan struct{}, 1),
	}
	s := &scripted{t: t, results: []StepResult{
		{Code: 1, Output: transientOutput},
		{Code: 0, Output: "ok"},
	}}
	clk := &fakeClock{}
	var waits []time.Duration
	base := clk.sleeper(&waits)
	allowRenewal := make(chan struct{}, 1)
	renewalWaitTimedOut := false
	sleep := func(ctx context.Context, d time.Duration) error {
		if d == LeaseRenewEvery {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-allowRenewal:
			}
		}
		return base(ctx, d)
	}
	exec := func(ctx context.Context, capture bool) (StepResult, error) {
		result, err := s.exec(ctx, capture)
		if err != nil {
			return result, err
		}
		if s.calls == 1 {
			api.firstAttemptDone.Store(true)
		}
		if s.calls == 2 {
			allowRenewal <- struct{}{}
			timer := time.NewTimer(10 * time.Second)
			defer timer.Stop()
			select {
			case <-api.renewedAfterFirstAttempt:
			case <-ctx.Done():
				return StepResult{}, ctx.Err()
			case <-timer.C:
				renewalWaitTimedOut = true
				return StepResult{}, errors.New("timed out waiting for renewal after first attempt")
			}
		}
		return result, nil
	}
	r := Run{
		Gate:         func(context.Context) (Verdict, error) { return Verdict{Deploy: true}, nil },
		Exec:         exec,
		Clock:        Clock{Wall: clk.wall, Monotonic: clk.monotonic},
		Sleep:        sleep,
		Lease:        lease(api),
		PulumiStacks: map[string]bool{"aws": true},
	}
	code := RunCD(context.Background(), "aws:up", r)
	if renewalWaitTimedOut {
		t.Fatal("timed out waiting for a renewal after the first attempt")
	}
	if code != 0 {
		t.Fatalf("RunCD = %d, want 0", code)
	}
	if got := holderWrites(api.writes); got < 2 {
		t.Fatalf("holder writes = %d, want the acquire and a renewal after the first attempt", got)
	}
}
