package cdgate

import (
	"context"
	"testing"
	"time"
)

// TestRunCDSkipAfterAcquireReleasesAndNeverDeploys: the first gate deploys and
// the Lease is taken; the re-check under the Lease says skip. The step exits 79
// (skip_exit_code), the gate ran twice, the command never ran, and the Lease is
// released.
func TestRunCDSkipAfterAcquireReleasesAndNeverDeploys(t *testing.T) {
	api := &memLease{record: &LeaseRecord{ResourceVersion: "1"}}
	clk := &fakeClock{}
	var waits []time.Duration
	gates := 0
	executed := false
	var skipped Verdict
	r := Run{
		Gate: func(context.Context) (Verdict, error) {
			gates++
			if gates == 1 {
				return Verdict{Deploy: true}, nil
			}
			return Verdict{Deploy: false, Reason: ReasonRestartNotHead}, nil
		},
		Skip: func(v Verdict) { skipped = v },
		Exec: func(context.Context, bool) (StepResult, error) {
			executed = true
			return StepResult{Code: 0}, nil
		},
		Clock: Clock{Wall: clk.wall, Monotonic: clk.monotonic},
		Sleep: clk.sleeper(&waits),
		Lease: lease(api),
	}
	if code := RunCD(context.Background(), "nix:deploy", r); code != 79 {
		t.Fatalf("RunCD = %d after a skip; want 79", code)
	}
	if gates != 2 {
		t.Fatalf("gate ran %d times; want 2 (first gate and in-Lease re-check)", gates)
	}
	if skipped.Reason != ReasonRestartNotHead {
		t.Fatalf("skip reason = %q", skipped.Reason)
	}
	if executed {
		t.Fatal("the command ran after the re-check said skip")
	}
	if len(api.writes) == 0 {
		t.Fatal("no Lease write recorded; want a release write")
	}
	if last := api.writes[len(api.writes)-1].HolderIdentity; last != "" {
		t.Fatalf("last Lease write holder = %q; want an empty holder (released)", last)
	}
}
