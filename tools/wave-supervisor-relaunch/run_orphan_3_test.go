package main

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func testRunRestartRequestsAndWedgeRecoveryWedgedOrphanBlockedByManagerRecovery(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.endpoints = testOrphanEndpoints
	h.probe = []bool{false, false, false, true}
	h.deps.Readiness.Poll = 2 * time.Minute
	stopped := false
	stop := h.deps.StopSlice
	h.deps.StopSlice = func(ctx context.Context) (bool, error) { stopped = true; return stop(ctx) }
	h.deps.Status = func(context.Context) (any, error) {
		if stopped {
			return map[string]any{"state": "retrying", "lastSweep": map[string]any{"completedAt": "done"}, "failures": []any{}}, nil
		}
		return map[string]any{"state": "idle", "lastSweep": map[string]any{"completedAt": "done"}, "failures": []any{}}, nil
	}
	got := runSupervisor(t.Context(), h.deps)
	if got.Reason != "manager-recovering" || !h.hasCall("stop-slice") || h.hasCall("restart") {
		t.Fatalf("run=%#v calls=%v", got, h.calls)
	}
}

func testRunRestartRequestsAndWedgeRecoveryWedgedOrphanAtThreeChargesStops(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.endpoints = testOrphanEndpoints
	h.probe = []bool{false, false, false}
	stamp := h.time.Add(-time.Minute).Format(time.RFC3339Nano)
	h.write(h.deps.Paths.State, fmt.Sprintf(`{"version":1,"spawns":[%q,%q,%q],"burned":[]}`, stamp, stamp, stamp))
	got := runSupervisor(t.Context(), h.deps)
	if got.Reason != "budget" || h.hasCall("stop-slice") {
		t.Fatalf("run=%#v calls=%v", got, h.calls)
	}
}
