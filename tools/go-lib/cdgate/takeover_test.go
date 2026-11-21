package cdgate

import (
	"context"
	"testing"
	"time"
)

// pulumiLockLine is the error pulumi prints when a stack lock is held. After a
// takeover the lock can be stale, so the step must fail on it once.
const pulumiLockLine = "error: the stack is currently locked by 1 lock(s). Either wait for the other process(es) to end or delete the lock file with `pulumi cancel`."

// TestRunCDLockAfterTakeoverFailsOnceAndReleases: a dead holder's record stays
// unchanged for a full LeaseDuration, so this step takes over. The stale Pulumi
// lock then fails the deploy once, with no retry, and the step releases the
// Lease it now holds.
func TestRunCDLockAfterTakeoverFailsOnceAndReleases(t *testing.T) {
	api := &memLease{record: &LeaseRecord{ResourceVersion: "1", HolderIdentity: "other/pod-9", RenewTime: "t0", LeaseTransitions: 1}}
	s := &scripted{t: t, results: []StepResult{{Code: 255, Output: pulumiLockLine}}}
	clk := &fakeClock{}
	var waits []time.Duration
	r := Run{
		Gate:         func(context.Context) (Verdict, error) { return Verdict{Deploy: true}, nil },
		Exec:         s.exec,
		Clock:        Clock{Wall: clk.wall, Monotonic: clk.monotonic},
		Sleep:        clk.sleeper(&waits),
		Lease:        lease(api),
		PulumiStacks: map[string]bool{"aws": true},
	}
	if code := RunCD(context.Background(), "aws:up", r); code != 255 {
		t.Fatalf("RunCD = %d, want the lock exit 255", code)
	}
	if s.calls != 1 {
		t.Fatalf("attempts = %d, want exactly 1: a stale lock must not be retried", s.calls)
	}
	var waited time.Duration
	for _, w := range waits {
		waited += w
	}
	if waited < LeaseDuration {
		t.Fatalf("took over after %v, want at least the full %v", waited, LeaseDuration)
	}
	if len(api.writes) < 2 {
		t.Fatalf("writes = %+v, want the takeover then the release", api.writes)
	}
	takeover := api.writes[0]
	if takeover.HolderIdentity != lease(api).HolderIdentity || takeover.LeaseTransitions != 2 {
		t.Fatalf("first write = %+v, want the takeover by this step with transitions 2", takeover)
	}
	if last := api.writes[len(api.writes)-1]; last.HolderIdentity != "" {
		t.Fatalf("final Lease write = %+v, want the release after the lock failure", last)
	}
}
