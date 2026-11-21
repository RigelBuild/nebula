package cdgate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/kubeserviceaccount"
)

// heldBy returns a Lease record held by holder.
func heldBy(holder string) *LeaseRecord {
	return &LeaseRecord{ResourceVersion: "1", HolderIdentity: holder}
}

func TestRenewUntilStoppedKeepsRenewingWhileHeld(t *testing.T) {
	api := &memLease{record: heldBy("42/step/pod-1")}
	clk := &fakeClock{}
	var waits []time.Duration
	stopAt := 3 * LeaseRenewEvery
	sleep := func(ctx context.Context, d time.Duration) error {
		if err := clk.sleeper(&waits)(ctx, d); err != nil {
			return err
		}
		if clk.now >= stopAt {
			return context.Canceled
		}
		return nil
	}
	got := RenewUntilStopped(context.Background(), lease(api), 0,
		Clock{Wall: clk.wall, Monotonic: clk.monotonic}, sleep, WallBound, func(error) {
			t.Fatal("renewal reported an error while the Lease was held")
		})
	if got.Lost {
		t.Fatal("renewal reported lost while the Lease was still held")
	}
	if len(api.writes) < 2 {
		t.Fatalf("writes = %d, want several renewals while held", len(api.writes))
	}
}

func TestRenewUntilStoppedReportsLossWhenAnotherHolderOwnsLease(t *testing.T) {
	api := &memLease{record: heldBy("99/other/pod-9")}
	clk := &fakeClock{}
	var waits []time.Duration
	got := RenewUntilStopped(context.Background(), lease(api), 0,
		Clock{Wall: clk.wall, Monotonic: clk.monotonic}, clk.sleeper(&waits), WallBound, func(error) {})
	if !got.Lost {
		t.Fatal("renewal did not report loss when another holder owns the Lease")
	}
	if len(api.writes) != 0 {
		t.Fatalf("renewal wrote %d times to a Lease it does not hold", len(api.writes))
	}
}

func TestRenewUntilStoppedLosesAfterDeadlineWithoutRenewal(t *testing.T) {
	// Every renewal fails with a non-loss error, so no renewal ever succeeds.
	// The loss deadline must end the loop after LeaseLossTimeout.
	boom := errors.New("apiserver unavailable")
	api := &memLease{record: heldBy("42/step/pod-1"), getErr: boom}
	clk := &fakeClock{}
	var waits []time.Duration
	var reported int
	got := RenewUntilStopped(context.Background(), lease(api), 0,
		Clock{Wall: clk.wall, Monotonic: clk.monotonic}, clk.sleeper(&waits), WallBound,
		func(error) { reported++ })
	if !got.Lost {
		t.Fatal("renewal did not report loss after the deadline passed with no success")
	}
	if clk.now < LeaseLossTimeout {
		t.Fatalf("lost after %v, want at least %v", clk.now, LeaseLossTimeout)
	}
	if reported == 0 {
		t.Fatal("renewal errors were not reported to the caller")
	}
}

func TestRenewUntilStoppedRetriesOneFailureWithoutLosing(t *testing.T) {
	// One renewal fails, then succeeds. The single failure must not stop renewal:
	// the caller sees the error once and renewal continues.
	boom := errors.New("transient")
	api := &memLease{record: heldBy("42/step/pod-1")}
	api.updateErrs = []error{boom}
	clk := &fakeClock{}
	var waits []time.Duration
	var reported []error
	sleep := func(ctx context.Context, d time.Duration) error {
		if err := clk.sleeper(&waits)(ctx, d); err != nil {
			return err
		}
		if clk.now >= 2*LeaseRenewEvery {
			return context.Canceled
		}
		return nil
	}
	got := RenewUntilStopped(context.Background(), lease(api), 0,
		Clock{Wall: clk.wall, Monotonic: clk.monotonic}, sleep, WallBound,
		func(err error) { reported = append(reported, err) })
	if got.Lost {
		t.Fatal("one transient renewal failure stopped renewal")
	}
	if len(reported) != 1 || !errors.Is(reported[0], boom) {
		t.Fatalf("reported = %v, want the one transient error", reported)
	}
}

func TestRenewUntilStoppedStopsOnCancel(t *testing.T) {
	api := &memLease{record: heldBy("42/step/pod-1")}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	clk := &fakeClock{}
	var waits []time.Duration
	got := RenewUntilStopped(ctx, lease(api), 0,
		Clock{Wall: clk.wall, Monotonic: clk.monotonic}, clk.sleeper(&waits), WallBound, func(error) {})
	if got.Lost {
		t.Fatal("a cancelled renewal reported loss")
	}
	if len(api.writes) != 0 {
		t.Fatal("a cancelled renewal still wrote the Lease")
	}
}

func TestReleaseLeaseClearsHolderWhenHeld(t *testing.T) {
	api := &memLease{record: heldBy("42/step/pod-1")}
	clk := &fakeClock{}
	var waits []time.Duration
	if err := ReleaseLease(context.Background(), lease(api), Clock{Wall: clk.wall, Monotonic: clk.monotonic}, clk.sleeper(&waits)); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	if len(api.writes) != 1 || api.writes[0].HolderIdentity != "" {
		t.Fatalf("writes = %+v, want one update clearing the holder", api.writes)
	}
}

func TestReleaseLeaseSkipsWhenAnotherHolds(t *testing.T) {
	api := &memLease{record: heldBy("99/other/pod-9")}
	clk := &fakeClock{}
	var waits []time.Duration
	if err := ReleaseLease(context.Background(), lease(api), Clock{Wall: clk.wall, Monotonic: clk.monotonic}, clk.sleeper(&waits)); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	if len(api.writes) != 0 {
		t.Fatalf("release wrote to a Lease held by another step: %+v", api.writes)
	}
}

func TestReleaseLeaseRetriesConflictsThenFailsAtLimit(t *testing.T) {
	// A release that keeps conflicting must fail the step, not spin forever.
	api := &memLease{record: heldBy("42/step/pod-1")}
	for range LeaseRetryLimit + 1 {
		api.updateErrs = append(api.updateErrs, conflict())
	}
	clk := &fakeClock{}
	var waits []time.Duration
	err := ReleaseLease(context.Background(), lease(api), Clock{Wall: clk.wall, Monotonic: clk.monotonic}, clk.sleeper(&waits))
	status, ok := errors.AsType[*kubeserviceaccount.StatusError](err)
	if !ok || status.Status != 409 {
		t.Fatalf("err = %v, want the final 409 after the retry budget", err)
	}
	if len(waits) != LeaseRetryLimit {
		t.Fatalf("backoff waits = %d, want %d", len(waits), LeaseRetryLimit)
	}
}

func TestReleaseLeaseRecoversFromTransientError(t *testing.T) {
	api := &memLease{record: heldBy("42/step/pod-1")}
	api.updateErrs = []error{errors.New("transient")}
	clk := &fakeClock{}
	var waits []time.Duration
	if err := ReleaseLease(context.Background(), lease(api), Clock{Wall: clk.wall, Monotonic: clk.monotonic}, clk.sleeper(&waits)); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	if len(waits) != 1 {
		t.Fatalf("waits = %v, want one backoff before the retry", waits)
	}
}
