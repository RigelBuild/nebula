package cdgate

import (
	"context"
	"testing"
	"time"
)

// TestAcquireWallClockJumpNeitherHastensNorDelaysTakeover: the takeover is timed
// on the monotonic clock. A wall-clock jump forward or back must not change when
// a contender takes over a stale holder. The wall clock here jumps by an hour
// partway through; the monotonic clock still advances by exactly the poll waits.
func TestAcquireWallClockJumpNeitherHastensNorDelaysTakeover(t *testing.T) {
	api := &memLease{record: &LeaseRecord{ResourceVersion: "1", HolderIdentity: "99/other/pod-9", RenewTime: "t0", LeaseTransitions: 2}}
	clk := &fakeClock{}
	var waits []time.Duration
	base := clk.sleeper(&waits)
	jumped := false
	sleep := func(ctx context.Context, d time.Duration) error {
		if !jumped {
			jumped = true
			clk.mu.Lock()
			clk.wallOffset += time.Hour
			clk.mu.Unlock()
		}
		return base(ctx, d)
	}
	sentAt, err := AcquireLease(context.Background(), lease(api), Clock{Wall: clk.wall, Monotonic: clk.monotonic}, sleep)
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	var waited time.Duration
	for _, w := range waits {
		waited += w
	}
	if waited < LeaseDuration {
		t.Fatalf("took over after %v of monotonic time, want at least the full %v despite the wall jump", waited, LeaseDuration)
	}
	if sentAt != clk.now {
		t.Fatalf("sentAt = %v, want the takeover write on the monotonic clock %v", sentAt, clk.now)
	}
}
