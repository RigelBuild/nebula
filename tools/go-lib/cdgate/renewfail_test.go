package cdgate

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/kubeserviceaccount"
)

// renewFailingLease lets the acquire's holder write through, then fails every
// later holder write, so renewal never succeeds. The empty-holder release still
// goes through, as a real API would.
type renewFailingLease struct {
	*memLease
	holderWrites int
}

func (f *renewFailingLease) Update(ctx context.Context, w LeaseWrite) (*LeaseRecord, error) {
	if w.HolderIdentity != "" {
		f.holderWrites++
		if f.holderWrites > 1 {
			return nil, &kubeserviceaccount.StatusError{Status: http.StatusInternalServerError}
		}
	}
	return f.memLease.Update(ctx, w)
}

// TestRunCDRenewalFailuresBeyond100sKillChildAndFailStep: every renewal write
// fails, so the Lease is never renewed. The child is stopped once the loss
// deadline passes, the step exits nonzero, and the final write releases the
// Lease with an empty holder.
func TestRunCDRenewalFailuresBeyond100sKillChildAndFailStep(t *testing.T) {
	api := &renewFailingLease{memLease: &memLease{record: &LeaseRecord{ResourceVersion: "1"}}}
	clk := &fakeClock{}
	var waits []time.Duration
	var started, stopped atomic.Bool
	exec := func(ctx context.Context, _ bool) (StepResult, error) {
		started.Store(true)
		<-ctx.Done()
		stopped.Store(true)
		return StepResult{Code: 143}, nil
	}
	r := Run{
		Gate:  func(context.Context) (Verdict, error) { return Verdict{Deploy: true}, nil },
		Exec:  exec,
		Clock: Clock{Wall: clk.wall, Monotonic: clk.monotonic},
		Sleep: clk.sleeper(&waits),
		Lease: lease(api),
	}
	code := RunCD(context.Background(), "nix:deploy", r)
	if code == 0 {
		t.Fatal("RunCD = 0 after renewal failed beyond the loss deadline; want a failing exit")
	}
	// A loss may land before the deploy starts, so the child need not run. The
	// safety rule is that a child which did start is stopped before RunCD returns.
	if started.Load() && !stopped.Load() {
		t.Fatal("the child started and was still running when RunCD returned after the loss")
	}
	if len(api.writes) == 0 {
		t.Fatal("no Lease write recorded")
	}
	if last := api.writes[len(api.writes)-1].HolderIdentity; last != "" {
		t.Fatalf("final Lease write holder = %q; want an empty holder (released)", last)
	}
}
