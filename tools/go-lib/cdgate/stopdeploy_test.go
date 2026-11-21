package cdgate

import (
	"context"
	"testing"
	"time"
)

// TestRunCDStopDuringDeployReleasesAfterChildExits: a stop during the deploy
// stops the child, and the Lease is released only after the child is gone. The
// executor records whether a release was written while it still ran.
func TestRunCDStopDuringDeployReleasesAfterChildExits(t *testing.T) {
	api := &memLease{record: &LeaseRecord{ResourceVersion: "1"}}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	childRunning := false
	releasedWhileRunning := false
	exec := func(ctx context.Context, _ bool) (StepResult, error) {
		childRunning = true
		stop() // the stop arrives while the child runs
		<-ctx.Done()
		childRunning = false
		return StepResult{Code: 143}, nil
	}
	r, _ := runFixture(t, Verdict{Deploy: true}, exec)
	r.Lease = lease(api)
	// Record release writes made while the child still runs.
	base := r.Sleep
	r.Sleep = func(ctx context.Context, d time.Duration) error {
		for _, w := range api.writes {
			if w.HolderIdentity == "" && childRunning {
				releasedWhileRunning = true
			}
		}
		return base(ctx, d)
	}
	if code := RunCD(ctx, "nix:deploy", r); code != 1 {
		t.Fatalf("RunCD = %d, want 1 after a stop interrupts the deploy", code)
	}
	if childRunning {
		t.Fatal("the child was still running when RunCD returned")
	}
	if releasedWhileRunning {
		t.Fatal("the Lease was released while the child still ran")
	}
	if last := api.writes[len(api.writes)-1]; last.HolderIdentity != "" {
		t.Fatalf("final Lease write = %+v, want the release after the child exited", last)
	}
}
