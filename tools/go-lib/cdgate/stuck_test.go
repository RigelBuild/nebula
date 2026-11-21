package cdgate

import (
	"context"
	"testing"
)

// stuckExec reports a deploy group that will not exit. Exited stays open until
// the test closes it, so the group is still running.
func stuckExec(exited chan struct{}) Executor {
	return func(context.Context, bool) (StepResult, error) {
		return StepResult{}, &DeployGroupStuckError{Exited: exited}
	}
}

// TestRunCDStuckGroupKeepsLeaseHeld: a deploy group that survives the stop
// keeps the Lease. Releasing it would let a contender deploy beside the
// survivor. RunCD runs in its own goroutine, and the test closes Exited only
// after the step has logged that it is holding, so no release is written early.
func TestRunCDStuckGroupKeepsLeaseHeld(t *testing.T) {
	exited := make(chan struct{})
	api := &memLease{record: &LeaseRecord{ResourceVersion: "1"}}
	r, _ := runFixture(t, Verdict{Deploy: true}, stuckExec(exited))
	r.Lease = lease(api)
	done := make(chan int, 1)
	go func() { done <- RunCD(context.Background(), "nix:deploy", r) }()

	// The group is still running: the step must not have released yet.
	// The stuck exit path blocks on Exited, so RunCD cannot return before it.
	select {
	case code := <-done:
		t.Fatalf("RunCD returned %d while the deploy group still ran; the Lease must stay held", code)
	default:
	}
	close(exited)
	if code := <-done; code == 0 {
		t.Fatalf("RunCD = 0 for a stuck deploy group; want a failing exit")
	}
	released := false
	for _, w := range api.writes {
		if w.HolderIdentity == "" {
			released = true
		}
	}
	if !released {
		t.Fatalf("the Lease was never released after the group exited: %+v", api.writes)
	}
}
