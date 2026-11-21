package cdgate

import (
	"context"
	"testing"
	"time"
)

// TestAcquireTakeover409sNeverExhaustBudget: a 409 on every takeover write is a
// lost race, not a failure. Eight conflicts in a row must not exhaust the retry
// budget, and the write after them must still succeed.
func TestAcquireTakeover409sNeverExhaustBudget(t *testing.T) {
	api := &memLease{record: &LeaseRecord{ResourceVersion: "1", HolderIdentity: ""}}
	// Refuse the first eight writes with a 409, then accept.
	api.updateErrs = []error{conflict(), conflict(), conflict(), conflict(), conflict(), conflict(), conflict(), conflict()}
	clk := &fakeClock{}
	var waits []time.Duration
	if _, err := AcquireLease(context.Background(), lease(api), Clock{Wall: clk.wall, Monotonic: clk.monotonic}, clk.sleeper(&waits)); err != nil {
		t.Fatalf("AcquireLease: %v; eight takeover 409s must not exhaust the budget", err)
	}
	if len(api.writes) != 9 {
		t.Fatalf("writes = %d, want eight 409s then one success", len(api.writes))
	}
}
