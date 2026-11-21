package cdgate

import (
	"context"
	"testing"
	"time"
)

// holderWrites counts the Lease writes that set a holder. An acquire and a
// renewal both set one; a release clears it.
func holderWrites(ws []LeaseWrite) int {
	n := 0
	for _, w := range ws {
		if w.HolderIdentity != "" {
			n++
		}
	}
	return n
}

// TestRunCDStuckGroupKeepsRenewing: a stuck group must keep the Lease renewed.
// A Lease that stops renewing expires, and a contender could deploy beside the
// survivor. The test proves renewal wrote during the stuck period.
func TestRunCDStuckGroupKeepsRenewing(t *testing.T) {
	exited := make(chan struct{})
	api := &memLease{record: &LeaseRecord{ResourceVersion: "1"}}
	clk := &fakeClock{}
	var waits []time.Duration
	base := clk.sleeper(&waits)
	closed := false
	// Renewal sleeps between writes. Once a renewal has landed while the group
	// is still stuck, let the group exit.
	sleep := func(ctx context.Context, d time.Duration) error {
		if !closed && holderWrites(api.writes) >= 2 {
			closed = true
			close(exited)
		}
		return base(ctx, d)
	}
	r := Run{
		Gate:  func(context.Context) (Verdict, error) { return Verdict{Deploy: true}, nil },
		Exec:  stuckExec(exited),
		Clock: Clock{Wall: clk.wall, Monotonic: clk.monotonic},
		Sleep: sleep,
		Lease: lease(api),
	}
	if code := RunCD(context.Background(), "nix:deploy", r); code == 0 {
		t.Fatalf("RunCD = 0 for a stuck deploy group; want a failing exit")
	}
	if !closed {
		t.Fatalf("the group never exited: renewal did not land a write while it ran (holder writes = %d)", holderWrites(api.writes))
	}
}

// TestRunCDLostLeaseDuringStuckGroupDoesNotRelease: when another holder takes
// the Lease while the group survives, the step must not release it, because it
// no longer owns it. Releasing would clear someone else's holder. The step fails.
func TestRunCDLostLeaseDuringStuckGroupDoesNotRelease(t *testing.T) {
	exited := make(chan struct{})
	api := &memLease{record: &LeaseRecord{ResourceVersion: "1"}}
	clk := &fakeClock{}
	var waits []time.Duration
	base := clk.sleeper(&waits)
	stolen := false
	// After the acquire, another holder replaces the record before the first
	// renewal reads it. renewOnce then sees a foreign holder and reports the loss.
	sleep := func(ctx context.Context, d time.Duration) error {
		if !stolen && holderWrites(api.writes) >= 1 {
			stolen = true
			api.record = &LeaseRecord{ResourceVersion: "2", HolderIdentity: "99/other/pod-9"}
		}
		return base(ctx, d)
	}
	r := Run{
		Gate:  func(context.Context) (Verdict, error) { return Verdict{Deploy: true}, nil },
		Exec:  stuckExec(exited),
		Clock: Clock{Wall: clk.wall, Monotonic: clk.monotonic},
		Sleep: sleep,
		Lease: lease(api),
	}
	done := make(chan int, 1)
	go func() { done <- RunCD(context.Background(), "nix:deploy", r) }()
	// Loss is what lets the stuck step return, so the group never needs to exit.
	code := <-done
	if code == 0 {
		t.Fatalf("RunCD = 0 after the Lease was lost; want a failing exit")
	}
	for _, w := range api.writes {
		if w.HolderIdentity == "" {
			t.Fatalf("the step released a Lease it had lost: %+v", api.writes)
		}
	}
}
