package main

import (
	"context"
	"testing"

	"github.com/RigelBuild/nebula/tools/go-lib/cotalreach"
	"github.com/RigelBuild/nebula/tools/go-lib/relaunch"
)

func testRunSpawnResult(t *testing.T, tc struct {
	name   string
	code   int
	want   string
	burned bool
}) {
	t.Helper()
	h := newRunHarness()
	h.spawnCodes = []int{tc.code, 0}
	if tc.want == "not-live" {
		h.liveAfter = false
	}
	if tc.code == cotalreach.NOT_REACHABLE || tc.want == "not-live" {
		h.deps.ProbeZellij = func(context.Context) (bool, error) { return true, nil }
	}
	got := runSupervisor(t.Context(), h.deps)
	if got.Reason != tc.want {
		t.Fatalf("run = %#v, want reason %q", got, tc.want)
	}
	state, valid := relaunch.ReadState(h.deps.Paths.State, h.fs)
	if !valid {
		t.Fatal("state invalid after run")
	}
	if tc.burned != (len(state.Burned) == 1) {
		t.Fatalf("burned = %v", state.Burned)
	}
	if tc.code == cotalreach.VIEW_FAILED {
		if got.Outcome != "relaunched" || got.Session != "supervisor-session" || h.countPrefix("spawn:") != 1 {
			t.Fatalf("view failure result=%#v spawn calls=%v", got, h.calls)
		}
	}
	if tc.code == cotalreach.SPAWN_REFUSED {
		if got.Outcome != "relaunched" || got.Session != "supervisor-session" || h.countPrefix("spawn:") != 2 {
			t.Fatalf("refused spawn retry result=%#v spawn calls=%v", got, h.calls)
		}
	}
	if tc.code == 2 && h.countPrefix("spawn:") != 1 {
		t.Fatalf("spawn usage calls=%v", h.calls)
	}
	if tc.code == cotalreach.SPAWN_REFUSED && h.countPrefix("spawn:") != 2 {
		t.Fatalf("spawn calls=%v", h.calls)
	}
}
