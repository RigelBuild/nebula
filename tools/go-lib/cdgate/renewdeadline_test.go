package cdgate

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// hungRenewal lets the acquire's read through. The first renewal read then hangs
// the way a stuck request does: it advances the fake clock to the 100s loss
// deadline, fires the deadline bound, and waits for the abort. It records that
// the abort happened.
type hungRenewal struct {
	*memLease
	clk      *fakeClock
	deadline *deadlineBound
	mu       sync.Mutex
	reads    int
	aborted  bool
}

func (h *hungRenewal) Get(ctx context.Context, name string) (*LeaseRecord, error) {
	h.mu.Lock()
	h.reads++
	hang := h.reads == 2
	h.mu.Unlock()
	if !hang {
		return h.memLease.Get(ctx, name)
	}
	// The request is still in flight when the loss deadline arrives.
	h.clk.advanceTo(LeaseLossTimeout)
	h.deadline.fire()
	<-ctx.Done()
	h.mu.Lock()
	h.aborted = true
	h.mu.Unlock()
	return nil, ctx.Err()
}

// deadlineBound is a Bound whose contexts are cancelled by fire. The hung read
// calls fire once the fake clock reaches the loss deadline.
type deadlineBound struct {
	mu      sync.Mutex
	cancels []context.CancelFunc
}

// bound returns the Bound. It is driven by fire, never by a real timer.
func (b *deadlineBound) bound() Bound {
	return func(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(ctx)
		b.mu.Lock()
		b.cancels = append(b.cancels, cancel)
		b.mu.Unlock()
		return ctx, cancel
	}
}

// fire cancels every in-flight bound.
func (b *deadlineBound) fire() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, cancel := range b.cancels {
		cancel()
	}
}

// TestRunCDRenewalInFlightAtDeadlineIsAbortedAndStopsChild: a renewal read still
// in flight at the 100s loss deadline is cancelled, and a child that started is
// stopped at that deadline, not later. The stop time is read from the fake clock.
func TestRunCDRenewalInFlightAtDeadlineIsAbortedAndStopsChild(t *testing.T) {
	clk := &fakeClock{}
	var deadline deadlineBound
	api := &hungRenewal{memLease: &memLease{record: &LeaseRecord{ResourceVersion: "1"}}, clk: clk, deadline: &deadline}
	var waits []time.Duration
	var started, stopped atomic.Bool
	var mu sync.Mutex
	var stoppedAt time.Duration
	exec := func(ctx context.Context, _ bool) (StepResult, error) {
		started.Store(true)
		<-ctx.Done()
		mu.Lock()
		stoppedAt = clk.monotonic()
		mu.Unlock()
		stopped.Store(true)
		return StepResult{Code: 143}, nil
	}
	r := Run{
		Gate:  func(context.Context) (Verdict, error) { return Verdict{Deploy: true}, nil },
		Exec:  exec,
		Clock: Clock{Wall: clk.wall, Monotonic: clk.monotonic},
		Sleep: clk.sleeper(&waits),
		Bound: deadline.bound(),
		Lease: lease(api),
	}
	if code := RunCD(context.Background(), "nix:deploy", r); code == 0 {
		t.Fatal("RunCD = 0 after the renewal read hung past the deadline; want a failing exit")
	}
	api.mu.Lock()
	aborted := api.aborted
	api.mu.Unlock()
	if !aborted {
		t.Fatal("the in-flight renewal read was never aborted")
	}
	// The loss can land during the in-Lease re-check, before any deploy starts, so
	// the child need not run. When it did run, it must stop at the loss deadline.
	if started.Load() {
		if !stopped.Load() {
			t.Fatal("the child started and was never stopped")
		}
		mu.Lock()
		defer mu.Unlock()
		if stoppedAt != LeaseLossTimeout {
			t.Fatalf("child stopped at %v, want exactly the %v loss deadline", stoppedAt, LeaseLossTimeout)
		}
	}
}
