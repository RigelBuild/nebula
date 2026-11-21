package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/cotalreach"
	"github.com/RigelBuild/nebula/tools/go-lib/relaunch"
)

func TestRunRestartRequestsAndWedgeRecovery(t *testing.T) {
	t.Run("stale, malformed and blank restart requests are deleted without restart", testRunRestartRequestsAndWedgeRecoveryStaleMalformedAndBlankRestartRequests)
	t.Run("RSS restart request targets matching owned server and does not stop slice", testRunRestartRequestsAndWedgeRecoveryRSSRestartRequestTargetsMatchingOwned)
	t.Run("RSS mismatch never restarts", testRunRestartRequestsAndWedgeRecoveryRSSMismatchNeverRestarts)
	t.Run("RSS request whose server changes before restart is discarded", testRunRestartRequestsAndWedgeRecoveryRSSRequestWhoseServerChangesBefore)
	t.Run("four viewer restarts bypass spawn budget and preserve charges", testRunRestartRequestsAndWedgeRecoveryFourViewerRestartsBypassSpawnBudget)
	t.Run("spent budget permits viewer restart but blocks spawn", testRunRestartRequestsAndWedgeRecoverySpentBudgetPermitsViewerRestartBut)
	t.Run("hold arising during wedge probes blocks viewer restart", testRunRestartRequestsAndWedgeRecoveryHoldArisingDuringWedgeProbesBlocks)
	t.Run("RSS request outside unit fails without stopping", testRunRestartRequestsAndWedgeRecoveryRSSRequestOutsideUnitFailsWithout)
	for _, tc := range []struct {
		name, identity, want string
	}{
		{name: "missing server", identity: "none", want: "zellij-down"},
		{name: "unreadable server", identity: "unavailable", want: "zellij-wedge-unverified"},
	} {
		t.Run("RSS request "+tc.name, func(t *testing.T) {
			h := newRunHarness()
			h.ps = testLivePS
			request := relaunch.RestartRequest{At: h.time.Format(time.RFC3339Nano), Reason: "rss", Cause: "rss", Identity: "500:1"}
			if err := relaunch.WriteRestartRequest(h.deps.Paths.RestartRequest, request, h.fs); err != nil {
				t.Fatal(err)
			}
			h.deps.ZellijIdentity = func(context.Context) string { return tc.identity }
			if got := runSupervisor(t.Context(), h.deps); got.Reason != tc.want || h.hasCall("restart") || h.hasCall("stop-slice") {
				t.Fatalf("run=%#v calls=%v", got, h.calls)
			}
		})
	}
	t.Run("healthy or orphaned supervisor never spawns to an uncapped server", testRunRestartRequestsAndWedgeRecoveryHealthyOrOrphanedSupervisorNeverSpawns)
	t.Run("exit 5 into a wedged server does not burn the session", testRunRestartRequestsAndWedgeRecoveryExit5IntoAWedgedServer)
	t.Run("RSS request gated by a hold is discarded", testRunRestartRequestsAndWedgeRecoveryRSSRequestGatedByAHold)
	t.Run("wedge request gated by a hold survives for the next run", testRunRestartRequestsAndWedgeRecoveryWedgeRequestGatedByAHold)
	t.Run("wedged server restarts then continues without stopping slice", testRunRestartRequestsAndWedgeRecoveryWedgedServerRestartsThenContinuesWithout)
	t.Run("wedged server outside unit fails closed", testRunRestartRequestsAndWedgeRecoveryWedgedServerOutsideUnitFailsClosed)
	t.Run("wedged server restart to uncapped identity fails closed and records fingerprint", testRunRestartRequestsAndWedgeRecoveryWedgedServerRestartToUncappedIdentity)
	t.Run("wedged orphan blocked by manager recovery does not restart", testRunRestartRequestsAndWedgeRecoveryWedgedOrphanBlockedByManagerRecovery)
	t.Run("wedged orphan with starting supervisor notifies instead of spawning", testRunRestartRequestsAndWedgeRecoveryWedgedOrphanWithStartingSupervisorNotifies)
	t.Run("wedged orphan enumerates proc once and charges once", testRunRestartRequestsAndWedgeRecoveryWedgedOrphanEnumeratesProcOnceAnd)
	t.Run("wedged orphan uses third spawn budget charge", testRunRestartRequestsAndWedgeRecoveryWedgedOrphanUsesThirdSpawnBudget)
	t.Run("wedged orphan at three charges stops nothing", testRunRestartRequestsAndWedgeRecoveryWedgedOrphanAtThreeChargesStops)
	t.Run("wedged orphan restart cannot use uncapped server", testRunRestartRequestsAndWedgeRecoveryWedgedOrphanRestartCannotUseUncapped)
	t.Run("wedged server failed restart does not spawn", testRunRestartRequestsAndWedgeRecoveryWedgedServerFailedRestartDoesNot)
	t.Run("wedged restart with replacement server outside unit does not spawn", testRunRestartRequestsAndWedgeRecoveryWedgedRestartWithReplacementServerOutside)
	t.Run("three stable failures allow spawn without restart", testRunRestartRequestsAndWedgeRecoveryThreeStableFailuresAllowSpawnWithout)
}

func testRunRestartRequestsAndWedgeRecoveryStaleMalformedAndBlankRestartRequests(t *testing.T) {
	t.Helper()
	now := time.UnixMilli(1_800_000_000_000).UTC()
	for _, body := range []string{
		fmt.Sprintf(`{"at":%q,"reason":"old"}`, now.Add(-31*time.Minute).Format(time.RFC3339Nano)),
		"{not json",
		fmt.Sprintf(`{"at":%q,"reason":"   "}`, now.Format(time.RFC3339Nano)),
	} {
		h := newRunHarness()
		h.ps = testLivePS
		h.write(h.deps.Paths.RestartRequest, body)
		_ = runSupervisor(t.Context(), h.deps)
		if _, exists := h.fs.files[h.deps.Paths.RestartRequest]; exists || h.hasCall("restart") {
			t.Fatalf("request retained or restarted: body=%q calls=%v", body, h.calls)
		}
	}
}

func testRunRestartRequestsAndWedgeRecoveryRSSRequestWhoseServerChangesBefore(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.ps = testLivePS
	request := relaunch.RestartRequest{At: h.time.Format(time.RFC3339Nano), Reason: "rss", Cause: "rss", Identity: "500:1"}
	if err := relaunch.WriteRestartRequest(h.deps.Paths.RestartRequest, request, h.fs); err != nil {
		t.Fatal(err)
	}
	identities := []string{"500:1", "700:3"}
	h.deps.ZellijIdentity = func(context.Context) string {
		if len(identities) == 0 {
			return "700:3"
		}
		identity := identities[0]
		identities = identities[1:]
		return identity
	}
	got := runSupervisor(t.Context(), h.deps)
	if got.Reason != "restart-not-needed" || h.hasCall("restart") || h.hasCall("stop-slice") {
		t.Fatalf("run=%#v calls=%v", got, h.calls)
	}
}

func testRunRestartRequestsAndWedgeRecoveryFourViewerRestartsBypassSpawnBudget(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.ps = testLivePS
	stamps := []string{
		h.time.Add(-time.Minute).Format(time.RFC3339Nano),
		h.time.Add(-2 * time.Minute).Format(time.RFC3339Nano),
		h.time.Add(-3 * time.Minute).Format(time.RFC3339Nano),
	}
	h.write(h.deps.Paths.State, fmt.Sprintf(`{"version":1,"spawns":[%q,%q,%q],"burned":[]}`, stamps[0], stamps[1], stamps[2]))
	for range 4 {
		request := relaunch.RestartRequest{At: h.time.Format(time.RFC3339Nano), Reason: "rss", Cause: "rss", Identity: h.identity}
		if err := relaunch.WriteRestartRequest(h.deps.Paths.RestartRequest, request, h.fs); err != nil {
			t.Fatal(err)
		}
		if got := runSupervisor(t.Context(), h.deps); got.Reason != "viewer-restarted" {
			t.Fatalf("viewer restart=%#v", got)
		}
	}
	state, valid := relaunch.ReadState(h.deps.Paths.State, h.fs)
	if !valid || len(state.Spawns) != 3 || h.countPrefix("restart") != 4 || h.countPrefix("spawn:") != 0 || h.hasCall("dm") || h.hasCall("status") {
		t.Fatalf("state=%#v valid=%t calls=%v", state, valid, h.calls)
	}
}

func testRunRestartRequestsAndWedgeRecoverySpentBudgetPermitsViewerRestartBut(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.probe = []bool{false, false, false, true}
	stamps := []string{
		h.time.Add(-time.Minute).Format(time.RFC3339Nano),
		h.time.Add(-2 * time.Minute).Format(time.RFC3339Nano),
		h.time.Add(-3 * time.Minute).Format(time.RFC3339Nano),
	}
	h.write(h.deps.Paths.State, fmt.Sprintf(`{"version":1,"spawns":[%q,%q,%q],"burned":[]}`, stamps[0], stamps[1], stamps[2]))
	got := runSupervisor(t.Context(), h.deps)
	if got.Reason != "budget" || !h.hasCall("restart") || h.countPrefix("spawn:") != 0 {
		t.Fatalf("run=%#v calls=%v", got, h.calls)
	}
}

func testRunRestartRequestsAndWedgeRecoveryHoldArisingDuringWedgeProbesBlocks(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.probe = []bool{false, false, false, true}
	sleep := h.deps.Sleep
	h.deps.Sleep = func(ctx context.Context, delay time.Duration) error {
		if err := sleep(ctx, delay); err != nil {
			return err
		}
		h.write(h.deps.Paths.Hold, fmt.Sprintf(`{"until":%q,"reason":"test"}`, h.time.Add(time.Hour).Format(time.RFC3339Nano)))
		return nil
	}
	if got := runSupervisor(t.Context(), h.deps); got.Reason != "hold" || h.hasCall("restart") || h.countPrefix("spawn:") != 0 {
		t.Fatalf("run=%#v calls=%v", got, h.calls)
	}
}

func testRunRestartRequestsAndWedgeRecoveryExit5IntoAWedgedServer(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.spawnCodes = []int{cotalreach.NOT_REACHABLE}
	h.liveAfter = false
	h.probe = []bool{true, false}
	got := runSupervisor(t.Context(), h.deps)
	state, valid := relaunch.ReadState(h.deps.Paths.State, h.fs)
	if got.Reason != "zellij-wedged" || !valid || len(state.Burned) != 0 {
		t.Fatalf("run=%#v state=%#v valid=%t", got, state, valid)
	}
}

func testRunRestartRequestsAndWedgeRecoveryWedgeRequestGatedByAHold(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.ps = testLivePS
	h.write(h.deps.Paths.Hold, fmt.Sprintf(`{"until":%q,"reason":"hold"}`, h.time.Add(time.Hour).Format(time.RFC3339Nano)))
	h.write(h.deps.Paths.RestartRequest, fmt.Sprintf(`{"at":%q,"reason":"test","cause":"wedge"}`, h.time.Format(time.RFC3339Nano)))
	if got := runSupervisor(t.Context(), h.deps); got.Reason != "hold" {
		t.Fatalf("outcome=%#v", got)
	}
	delete(h.fs.files, h.deps.Paths.Hold)
	h.probe = []bool{false, false, false, true}
	_ = runSupervisor(t.Context(), h.deps)
	if !h.hasCall("restart") {
		t.Fatalf("wedge request did not survive hold: %v", h.calls)
	}
}

func testRunRestartRequestsAndWedgeRecoveryWedgedServerRestartToUncappedIdentity(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.probe = []bool{false, false, false, true}
	restarted := false
	restart := h.deps.RestartZellij
	h.deps.RestartZellij = func(ctx context.Context) (bool, error) {
		restarted = true
		return restart(ctx)
	}
	h.deps.Trigger = func(context.Context) (Trigger, error) {
		server := "500:1"
		if restarted {
			server = "700:3"
		}
		return Trigger{BootID: "boot", ManagerUnit: "cotal-manager.service", Units: []UnitFact{{Unit: "cotal-manager.service", InvocationID: "manager", ActiveState: "active"}}, ZellijServer: server}, nil
	}
	got := runSupervisor(t.Context(), h.deps)
	state, valid := relaunch.ReadState(h.deps.Paths.State, h.fs)
	if got.Reason != "zellij-restart-uncapped" || !valid || state.LastFingerprint != "boot:manager:devbox=700:3" || !h.hasCall("restart") || h.countPrefix("spawn:") != 0 {
		t.Fatalf("run=%#v state=%#v valid=%t calls=%v", got, state, valid, h.calls)
	}
}

func testRelaunchCommandCLIRestartFlagsPreserveFreeTextAnd(t *testing.T) {
	t.Helper()
	for _, args := range [][]string{
		{"request-zellij-restart", "--reason", "reason with spaces and --flags", "--rss", "000500:0001"},
		{"request-zellij-restart", "--rss", "000500:0001", "--reason", "reason with spaces and --flags"},
	} {
		h := newRunHarness()
		if got := runCommandForTest(t.Context(), args, h.deps); got != 0 {
			t.Fatalf("command(%v)=%d", args, got)
		}
		request, stale, err := relaunch.ReadRestartRequest(h.deps.Paths.RestartRequest, h.time, h.fs)
		if err != nil || stale || request == nil || request.Cause != "rss" || request.Identity != "000500:0001" || request.Reason != "reason with spaces and --flags" {
			t.Fatalf("request=%#v stale=%t err=%v", request, stale, err)
		}
	}
}

func testRunRestartRequestsAndWedgeRecoveryRSSRestartRequestTargetsMatchingOwned(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.ps = testLivePS
	h.installWaveScope()
	request := relaunch.RestartRequest{At: h.time.Format(time.RFC3339Nano), Reason: "rss", Cause: "rss", Identity: "500:1"}
	if err := relaunch.WriteRestartRequest(h.deps.Paths.RestartRequest, request, h.fs); err != nil {
		t.Fatal(err)
	}
	h.probe = []bool{true}
	got := runSupervisor(t.Context(), h.deps)
	if got.Reason != "viewer-restarted" || !h.hasCall("restart") || h.hasCall("stop-slice") || h.countPrefix("spawn:") != 0 || h.hasCall("dm") {
		t.Fatalf("run=%#v calls=%v", got, h.calls)
	}
}

func testRunRestartRequestsAndWedgeRecoveryRSSMismatchNeverRestarts(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.ps = testLivePS
	request := relaunch.RestartRequest{At: h.time.Format(time.RFC3339Nano), Reason: "rss", Cause: "rss", Identity: "499:1"}
	if err := relaunch.WriteRestartRequest(h.deps.Paths.RestartRequest, request, h.fs); err != nil {
		t.Fatal(err)
	}
	if got := runSupervisor(t.Context(), h.deps); got.Reason != "restart-not-needed" || h.hasCall("restart") {
		t.Fatalf("run=%#v calls=%v", got, h.calls)
	}
}

func testRunRestartRequestsAndWedgeRecoveryRSSRequestOutsideUnitFailsWithout(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.ps = testLivePS
	h.unitProcs = "900\n"
	request := relaunch.RestartRequest{At: h.time.Format(time.RFC3339Nano), Reason: "rss", Cause: "rss", Identity: "500:1"}
	if err := relaunch.WriteRestartRequest(h.deps.Paths.RestartRequest, request, h.fs); err != nil {
		t.Fatal(err)
	}
	if got := runSupervisor(t.Context(), h.deps); got.Reason != "zellij-wedged-outside-unit" || h.hasCall("stop-slice") {
		t.Fatalf("run=%#v calls=%v", got, h.calls)
	}
}

func testRunRestartRequestsAndWedgeRecoveryRSSRequestGatedByAHold(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.ps = testLivePS
	h.write(h.deps.Paths.Hold, fmt.Sprintf(`{"until":%q,"reason":"hold"}`, h.time.Add(time.Hour).Format(time.RFC3339Nano)))
	h.write(h.deps.Paths.RestartRequest, fmt.Sprintf(`{"at":%q,"reason":"test","cause":"rss","identity":"500:1"}`, h.time.Format(time.RFC3339Nano)))
	if got := runSupervisor(t.Context(), h.deps); got.Reason != "hold" {
		t.Fatalf("outcome=%#v", got)
	}
	delete(h.fs.files, h.deps.Paths.Hold)
	_ = runSupervisor(t.Context(), h.deps)
	if h.hasCall("restart") {
		t.Fatalf("RSS request survived hold: %v", h.calls)
	}
}

func testRunRestartRequestsAndWedgeRecoveryWedgedServerRestartsThenContinuesWithout(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.probe = []bool{false, false, false, true}
	got := runSupervisor(t.Context(), h.deps)
	if got.Outcome != "relaunched" || !h.hasCall("restart") || h.countPrefix("spawn:") != 1 || h.hasCall("stop-slice") {
		t.Fatalf("run=%#v calls=%v", got, h.calls)
	}
}

func testRunRestartRequestsAndWedgeRecoveryWedgedServerOutsideUnitFailsClosed(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.probe = []bool{false, false, false}
	h.unitProcs = "900\n"
	if got := runSupervisor(t.Context(), h.deps); got.Reason != "zellij-wedged-outside-unit" || h.hasCall("stop-slice") {
		t.Fatalf("run=%#v calls=%v", got, h.calls)
	}
}

func testRunRestartRequestsAndWedgeRecoveryWedgedServerFailedRestartDoesNot(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.probe = []bool{false, false, false}
	h.deps.RestartZellij = func(context.Context) (bool, error) { return false, nil }
	if got := runSupervisor(t.Context(), h.deps); got.Reason != "zellij-restart-failed" || h.countPrefix("spawn:") != 0 {
		t.Fatalf("run=%#v calls=%v", got, h.calls)
	}
}

func testRunRestartRequestsAndWedgeRecoveryWedgedRestartWithReplacementServerOutside(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.probe = []bool{false, false, false, true}
	h.deps.Trigger = func(context.Context) (Trigger, error) {
		return Trigger{BootID: "boot", ManagerUnit: "cotal-manager.service", Units: []UnitFact{{Unit: "cotal-manager.service", InvocationID: "manager", ActiveState: "active"}}, ZellijServer: "700:3"}, nil
	}
	if got := runSupervisor(t.Context(), h.deps); got.Reason != "zellij-restart-uncapped" || h.countPrefix("spawn:") != 0 {
		t.Fatalf("run=%#v calls=%v", got, h.calls)
	}
}

func testRunRestartRequestsAndWedgeRecoveryThreeStableFailuresAllowSpawnWithout(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.identity = "none"
	h.probe = []bool{false, false, false, false}
	got := runSupervisor(t.Context(), h.deps)
	state, valid := relaunch.ReadState(h.deps.Paths.State, h.fs)
	if got.Outcome != "relaunched" || got.Reason != "absent" || got.Session != "supervisor-session" || !valid || len(state.Burned) != 0 || h.countPrefix("spawn:") != 1 || h.hasCall("restart") {
		t.Fatalf("run=%#v state=%#v valid=%t calls=%v", got, state, valid, h.calls)
	}
}

func testRelaunchCommandCLIRequestStoresRestartDataAndStarts(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	if got := runCommandForTest(t.Context(), []string{"request-zellij-restart", "--rss", "500:1", "--reason", "rss"}, h.deps); got != 0 {
		t.Fatalf("command status=%d", got)
	}
	if !h.hasCall("start-relaunch") {
		t.Fatalf("calls=%v", h.calls)
	}
	request, stale, err := relaunch.ReadRestartRequest(h.deps.Paths.RestartRequest, h.time, h.fs)
	if err != nil || stale || request == nil || request.Cause != "rss" || request.Identity != "500:1" {
		t.Fatalf("request=%#v stale=%t err=%v", request, stale, err)
	}
}

func testRelaunchCommandCLIRestartDefaultsCauseToWedgeAnd(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.ps = testLivePS
	if got := runCommandForTest(t.Context(), []string{"request-zellij-restart", "--reason", "drill"}, h.deps); got != 0 {
		t.Fatalf("command status=%d", got)
	}
	request, stale, err := relaunch.ReadRestartRequest(h.deps.Paths.RestartRequest, h.time, h.fs)
	if err != nil || stale || request == nil || request.Cause != "wedge" {
		t.Fatalf("request=%#v stale=%t err=%v", request, stale, err)
	}
	if got := runSupervisor(t.Context(), h.deps); got.Reason != "restart-not-needed" {
		t.Fatalf("run=%#v", got)
	}
	if _, exists := h.fs.files[h.deps.Paths.RestartRequest]; exists {
		t.Fatal("healthy restart request remains")
	}
}

func testRelaunchCommandCLIRestartServiceFailureRemovesRequest(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.deps.StartRelaunch = func(context.Context) (bool, error) { return false, nil }
	if got := runCommandForTest(t.Context(), []string{"request-zellij-restart", "--reason", "test"}, h.deps); got != 1 {
		t.Fatalf("command status=%d", got)
	}
	if _, ok := h.fs.files[h.deps.Paths.RestartRequest]; ok {
		t.Fatal("request remains after service failure")
	}
}

func testRelaunchCommandCLIRestartStartupErrorCleansRequestAnd(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	startErr := errors.New("start failed")
	h.deps.StartRelaunch = func(context.Context) (bool, error) { return false, startErr }
	root := newRootCommand(t.Context(), h.deps, io.Discard, io.Discard)
	root.SetArgs([]string{"request-zellij-restart", "--reason", "test"})
	err := root.ExecuteContext(context.Background())
	if !errors.Is(err, startErr) {
		t.Fatalf("error=%v", err)
	}
	if _, exists := h.fs.files[h.deps.Paths.RestartRequest]; exists {
		t.Fatal("restart request remains after failed service start")
	}
}
