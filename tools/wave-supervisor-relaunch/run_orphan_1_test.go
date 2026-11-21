package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/cotalreach"
	"github.com/RigelBuild/nebula/tools/go-lib/relaunch"
)

func TestRunOrphanAndSpawnFlow(t *testing.T) {
	t.Run("stable orphan census is persisted before stop and spawn", testRunOrphanAndSpawnFlowStableOrphanCensusIsPersistedBefore)
	t.Run("orphan relaunch with unreadable devbox is not reported again", testRunOrphanAndSpawnFlowOrphanRelaunchWithUnreadableDevboxIs)
	t.Run("wedged orphan charges before final classification while ordinary orphan charges after stability", testRunOrphanAndSpawnFlowWedgedOrphanChargesBeforeFinalClassification)
	t.Run("hold appearing during stability prevents charge and stop", testRunOrphanAndSpawnFlowHoldAppearingDuringStabilityPreventsCharge)
	t.Run("healthy supervisor beside orphan workers never stops or writes a manifest", testRunOrphanAndSpawnFlowHealthySupervisorBesideOrphanWorkersNever)
	t.Run("lane created during stop is added to manifest before spawn", testRunOrphanAndSpawnFlowLaneCreatedDuringStopIsAdded)
	t.Run("stable supervisor lanes do not stop the wave", testRunOrphanAndSpawnFlowStableSupervisorLanesDoNotStop)
	t.Run("starting supervisor does not stop or spawn", testRunOrphanAndSpawnFlowStartingSupervisorDoesNotStopOr)
	t.Run("missing session fails without spawning", testRunOrphanAndSpawnFlowMissingSessionFailsWithoutSpawning)
	t.Run("missing supervisor prompt does not reference a stale manifest", testRunOrphanAndSpawnFlowMissingSupervisorPromptDoesNotReference)
	t.Run("starting supervisor at the stability recheck skips before census", testRunOrphanAndSpawnFlowStartingSupervisorAtTheStabilityRecheck)
	t.Run("malformed ps at the stability recheck fails before census", testRunOrphanAndSpawnFlowMalformedPsAtTheStabilityRecheck)
	t.Run("malformed initial ps fails without stopping", testRunOrphanAndSpawnFlowMalformedInitialPsFailsWithoutStopping)
	t.Run("manager, nemo and a lane-only starting process never stop the wave", testRunOrphanAndSpawnFlowManagerNemoAndALaneOnly)
	t.Run("orphan removed at the stability recheck skips", testRunOrphanAndSpawnFlowOrphanRemovedAtTheStabilityRecheck)
	t.Run("orphan gains worker at stability recheck skips", testRunOrphanAndSpawnFlowOrphanGainsWorkerAtStabilityRecheck)
	t.Run("final pre-stop sample sees supervisor starting", testRunOrphanAndSpawnFlowFinalPreStopSampleSeesSupervisor)
	t.Run("final pre-stop malformed ps fails closed", testRunOrphanAndSpawnFlowFinalPreStopMalformedPsFails)
	t.Run("final pre-stop supervisor live cancels stop", testRunOrphanAndSpawnFlowFinalPreStopSupervisorLiveCancels)
	t.Run("final pre-stop sample gaining a lane cancels stop", testRunOrphanAndSpawnFlowFinalPreStopSampleGainingA)
	for _, tc := range []struct {
		name   string
		code   int
		want   string
		burned bool
	}{
		{name: "view failure counts as spawned", code: cotalreach.VIEW_FAILED, want: "absent"},
		{name: "spawn refused retries once", code: cotalreach.SPAWN_REFUSED, want: "absent"},
		{name: "unreachable burns session", code: cotalreach.NOT_REACHABLE, want: "no-session", burned: true},
		{name: "exit 1 burns the session", code: 1, want: "no-session", burned: true},
		{name: "exit 2 reports usage without burning session", code: 2, want: "spawn-usage"},
		{name: "postcondition burns session", code: 0, want: "not-live", burned: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testRunSpawnResult(t, tc)
		})
	}
	t.Run("a spawn proceeds with devbox down", testRunOrphanAndSpawnFlowASpawnProceedsWithDevboxDown)
	t.Run("hold appearing before the spawn prevents a budget charge", testRunOrphanAndSpawnFlowHoldAppearingBeforeTheSpawnPrevents)
	t.Run("unexpected nonzero spawn racing to live is skipped", testRunOrphanAndSpawnFlowUnexpectedNonzeroSpawnRacingToLive)
	t.Run("unexpected nonzero spawn racing to live beside orphan lane is skipped", testRunOrphanAndSpawnFlowUnexpectedNonzeroSpawnRacingToLiveBesideOrphanLane)
	t.Run("manager invocation change after a refused spawn is not burned", testRunOrphanAndSpawnFlowManagerInvocationChangeAfterARefused)
	t.Run("manager invocation change after accepted spawn postcondition is not burned", testRunOrphanAndSpawnFlowManagerInvocationChangeAfterAcceptedSpawn)
	t.Run("wedged server after failed postcondition is not burned", testRunOrphanAndSpawnFlowWedgedServerAfterFailedPostconditionIs)
	t.Run("spawn budget caps attempts", testRunOrphanAndSpawnFlowSpawnBudgetCapsAttempts)
	t.Run("failed slice stop returns wave-stop without spawning", testRunOrphanAndSpawnFlowFailedSliceStopReturnsWaveStop)
	t.Run("orphan still present through stop deadline returns presence-stuck", testRunOrphanAndSpawnFlowOrphanStillPresentThroughStopDeadline)
	t.Run("exit 5 after manager restarts during spawn does not burn session", testRunOrphanAndSpawnFlowExit5AfterManagerRestartsDuring)
	t.Run("spawn retry sees uncapped server and does not retry", testRunOrphanAndSpawnFlowSpawnRetrySeesUncappedServerAnd)
	t.Run("orphan stability sleep cancellation fails closed before stop", testRunOrphanAndSpawnFlowOrphanStabilitySleepCancellationFailsClosed)
	t.Run("orphan with missing session fails before stopping", testRunOrphanAndSpawnFlowOrphanWithMissingSessionFailsBefore)
	t.Run("orphan census unmapped process fails closed before stopping", testRunOrphanAndSpawnFlowOrphanCensusUnmappedProcessFailsClosed)
	t.Run("stop runner failure is best effort before slice stop", testRunOrphanAndSpawnFlowStopRunnerFailureIsBestEffort)
	t.Run("manager seat stops precede the slice stop", testRunOrphanAndSpawnFlowManagerSeatStopsPrecedeTheSlice)
}

func testRunOrphanAndSpawnFlowStableOrphanCensusIsPersistedBefore(t *testing.T) {
	h := newRunHarness()
	h.installWaveScope()
	h.endpoints = testOrphanEndpoints
	start := h.time
	stopSlice := h.deps.StopSlice
	var stoppedAfter time.Duration
	h.deps.StopSlice = func(ctx context.Context) (bool, error) {
		stoppedAfter = h.time.Sub(start)
		return stopSlice(ctx)
	}
	got := runSupervisor(t.Context(), h.deps)
	if got.Outcome != "relaunched" || got.State != "orphaned" || got.Session != "census-supervisor" || got.Mode != "continue" {
		t.Fatalf("run = %#v", got)
	}
	if !h.hasCall("stop-slice") {
		t.Fatalf("calls = %v", h.calls)
	}
	h.requireSingleSpawnPrefix(t, "spawn:foreman --continue census-supervisor --view-session viewer --prompt")
	if stoppedAfter < orphanStable {
		t.Fatalf("stop occurred after %s, want at least %s", stoppedAfter, orphanStable)
	}
	if _, ok := h.fs.files[h.deps.Paths.Manifest]; !ok {
		t.Fatal("orphan census manifest was not written")
	}
	firstManifestRename := -1
	lastManifestRename := -1
	stop, spawn := -1, -1
	for index, event := range h.events {
		switch event {
		case "manifest-rename":
			if firstManifestRename < 0 {
				firstManifestRename = index
			}
			lastManifestRename = index
		case "stop-slice":
			stop = index
		case "spawn":
			spawn = index
		}
	}
	if firstManifestRename < 0 || stop < 0 || spawn < 0 || firstManifestRename >= stop || stop >= lastManifestRename || lastManifestRename >= spawn {
		t.Fatalf("event order = %v", h.events)
	}
	state, valid := relaunch.ReadState(h.deps.Paths.State, h.fs)
	if !valid || len(state.Spawns) != 1 {
		t.Fatalf("charged state = %#v, valid=%t", state, valid)
	}
}

func testRunOrphanAndSpawnFlowOrphanRelaunchWithUnreadableDevboxIs(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.endpoints = testOrphanEndpoints
	h.identity = "unavailable"
	h.installWaveScope()
	if got := runSupervisor(t.Context(), h.deps); got.Outcome != "relaunched" {
		t.Fatalf("initial orphan recovery = %#v", got)
	}
	h.endpoints = ""
	if got := runSupervisor(t.Context(), h.deps); got.Reason != "devbox-unavailable" {
		t.Fatalf("follow-up run = %#v", got)
	}
	if h.hasCall("dm") {
		t.Fatalf("unreadable scan triggered a repeat notice: %v", h.calls)
	}
}

func testRunOrphanAndSpawnFlowWedgedOrphanChargesBeforeFinalClassification(t *testing.T) {
	t.Helper()
	for _, tc := range []struct {
		name    string
		wedged  bool
		wantLen int
	}{
		{name: "ordinary path", wantLen: 0},
		{name: "wedged path", wedged: true, wantLen: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRunHarness()
			h.endpoints = testOrphanEndpoints
			if tc.wedged {
				h.probe = []bool{false, false, false, true, true}
			}
			var classifications int
			originalPS := h.deps.PS
			h.deps.PS = func(ctx context.Context) (string, error) {
				classifications++
				if classifications == 3 {
					state, valid := relaunch.ReadState(h.deps.Paths.State, h.fs)
					if !valid || len(state.Spawns) != tc.wantLen {
						return "", fmt.Errorf("classification observed %d spawn charges, want %d", len(state.Spawns), tc.wantLen)
					}
				}
				return originalPS(ctx)
			}
			if got := runSupervisor(t.Context(), h.deps); got.Outcome != "relaunched" {
				t.Fatalf("run = %#v", got)
			}
		})
	}
}

func testRunOrphanAndSpawnFlowHoldAppearingDuringStabilityPreventsCharge(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.endpoints = testOrphanEndpoints
	originalSleep := h.deps.Sleep
	h.deps.Sleep = func(ctx context.Context, delay time.Duration) error {
		if err := originalSleep(ctx, delay); err != nil {
			return err
		}
		h.write(h.deps.Paths.Hold, fmt.Sprintf(`{"until":%q,"reason":"operator"}`, h.time.Add(time.Minute).Format(time.RFC3339Nano)))
		return nil
	}
	if got := runSupervisor(t.Context(), h.deps); got.Reason != "hold" || h.hasCall("stop-slice") {
		t.Fatalf("run=%#v calls=%v", got, h.calls)
	}
	state, valid := relaunch.ReadState(h.deps.Paths.State, h.fs)
	if !valid || len(state.Spawns) != 0 {
		t.Fatalf("charged state=%#v valid=%t", state, valid)
	}
}

func testRunOrphanAndSpawnFlowHealthySupervisorBesideOrphanWorkersNever(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.ps = testLivePS
	h.endpoints = "runners/rigel-runners-2 agent ○ idle\naccounts/worker agent ○ idle"
	for range 3 {
		if got := runSupervisor(t.Context(), h.deps); got.State != "live" || h.hasCall("stop-slice") {
			t.Fatalf("run=%#v calls=%v", got, h.calls)
		}
	}
	if _, exists := h.fs.files[h.deps.Paths.Manifest]; exists {
		t.Fatal("manifest written for healthy supervisor")
	}
}

func testRunOrphanAndSpawnFlowLaneCreatedDuringStopIsAdded(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.endpoints = testOrphanEndpoints
	h.installWaveScope()
	h.deps.EndpointsClear = func(context.Context) (bool, error) {
		h.fs.files["/sessions/accounts.jsonl"] = []byte(`{"type":"session","id":"accounts-session","title":"accounts"}`)
		h.fs.times["/sessions/accounts.jsonl"] = time.UnixMilli(2)
		return true, nil
	}
	h.deps.SeatNames = func(context.Context) (map[string]struct{}, error) {
		return map[string]struct{}{"foreman": {}, "accounts": {}}, nil
	}
	spawn := h.deps.Spawn
	h.deps.Spawn = func(ctx context.Context, args []string, cwd string) (int, error) {
		var manifest WaveManifest
		if err := json.Unmarshal(h.fs.files[h.deps.Paths.Manifest], &manifest); err != nil {
			return -1, fmt.Errorf("decode manifest before spawn: %w", err)
		}
		found := false
		for _, agent := range manifest.Agents {
			if agent.Name == "accounts" && agent.SessionID == "accounts-session" {
				found = true
				break
			}
		}
		if !found {
			return -1, fmt.Errorf("manifest missing reconciled account session: %#v", manifest)
		}
		return spawn(ctx, args, cwd)
	}
	if got := runSupervisor(t.Context(), h.deps); got.Outcome != "relaunched" {
		t.Fatalf("run=%#v", got)
	}
}

func testRunOrphanAndSpawnFlowMissingSupervisorPromptDoesNotReference(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.write(h.deps.Paths.Manifest, `{"at":"old","fingerprint":"old","agents":[{"name":"accounts","sessionId":"stale"}]}`)
	var prompt string
	h.deps.Spawn = func(_ context.Context, args []string, _ string) (int, error) {
		for index, arg := range args {
			if arg == "--prompt" && index+1 < len(args) {
				prompt = args[index+1]
			}
		}
		h.ps = testLivePS
		return 0, nil
	}
	if got := runSupervisor(t.Context(), h.deps); got.Outcome != "relaunched" {
		t.Fatalf("run = %#v", got)
	}
	if strings.Contains(prompt, "wave-relaunch-manifest.json") || !strings.Contains(prompt, "Lanes relaunch from /state/wave-seats.json; do not spawn them.") {
		t.Fatalf("spawn prompt = %q", prompt)
	}
}

func testRunOrphanAndSpawnFlowStartingSupervisorAtTheStabilityRecheck(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.endpoints = testOrphanEndpoints
	var calls int
	h.deps.PS = func(context.Context) (string, error) {
		calls++
		if calls == 1 {
			return testPSEnvelope(""), nil
		}
		return testPSEnvelope(`{"name":"foreman","mesh":"starting"}`), nil
	}
	got := runSupervisor(t.Context(), h.deps)
	if got.Reason != "starting" || h.hasCall("stop-slice") || h.countPrefix("spawn:") != 0 {
		t.Fatalf("run=%#v calls=%v", got, h.calls)
	}
	if _, exists := h.fs.files[h.deps.Paths.Manifest]; exists {
		t.Fatal("manifest written before stability recheck passed")
	}
}

func testRunOrphanAndSpawnFlowMalformedPsAtTheStabilityRecheck(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.endpoints = testOrphanEndpoints
	var calls int
	h.deps.PS = func(context.Context) (string, error) {
		calls++
		if calls == 1 {
			return testPSEnvelope(""), nil
		}
		return "not json", nil
	}
	got := runSupervisor(t.Context(), h.deps)
	if got.Reason != "ps-invalid" || h.hasCall("stop-slice") {
		t.Fatalf("run=%#v calls=%v", got, h.calls)
	}
	if _, exists := h.fs.files[h.deps.Paths.Manifest]; exists {
		t.Fatal("manifest written before malformed stability sample was rejected")
	}
}

func testRunOrphanAndSpawnFlowOrphanRemovedAtTheStabilityRecheck(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	var calls int
	h.deps.Endpoints = func(context.Context) (string, error) {
		calls++
		if calls == 2 {
			return "", nil
		}
		return testOrphanEndpoints, nil
	}
	got := runSupervisor(t.Context(), h.deps)
	if got.Reason != "orphan-unstable" || h.hasCall("stop-slice") || h.countPrefix("spawn:") != 0 {
		t.Fatalf("run=%#v calls=%v", got, h.calls)
	}
}

func testRunOrphanAndSpawnFlowOrphanGainsWorkerAtStabilityRecheck(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	var calls int
	h.deps.Endpoints = func(context.Context) (string, error) {
		calls++
		if calls == 2 {
			return testOrphanEndpoints + "\naccounts/worker agent ○ idle", nil
		}
		return testOrphanEndpoints, nil
	}
	got := runSupervisor(t.Context(), h.deps)
	if got.Reason != "orphan-unstable" || h.hasCall("stop-slice") {
		t.Fatalf("run=%#v calls=%v", got, h.calls)
	}
}

func testRunOrphanAndSpawnFlowFinalPreStopSampleSeesSupervisor(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.endpoints = testOrphanEndpoints
	var calls int
	h.deps.PS = func(context.Context) (string, error) {
		calls++
		if calls < 3 {
			return testPSEnvelope(""), nil
		}
		return testPSEnvelope(`{"name":"foreman","mesh":"starting"}`), nil
	}
	got := runSupervisor(t.Context(), h.deps)
	if got.Reason != "starting" || h.hasCall("stop-slice") {
		t.Fatalf("run=%#v calls=%v", got, h.calls)
	}
}

func testRunOrphanAndSpawnFlowFinalPreStopMalformedPsFails(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.endpoints = testOrphanEndpoints
	var calls int
	h.deps.PS = func(context.Context) (string, error) {
		calls++
		if calls < 3 {
			return testPSEnvelope(""), nil
		}
		return "not json", nil
	}
	got := runSupervisor(t.Context(), h.deps)
	if got.Reason != "ps-invalid" || h.hasCall("stop-slice") {
		t.Fatalf("run=%#v calls=%v", got, h.calls)
	}
}

func testRunOrphanAndSpawnFlowFinalPreStopSupervisorLiveCancels(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.endpoints = testOrphanEndpoints
	var calls int
	h.deps.PS = func(context.Context) (string, error) {
		calls++
		if calls < 3 {
			return testPSEnvelope(""), nil
		}
		return testPSEnvelope(testLivePS), nil
	}
	got := runSupervisor(t.Context(), h.deps)
	if got.Reason != "orphan-unstable" || h.hasCall("stop-slice") {
		t.Fatalf("run=%#v calls=%v", got, h.calls)
	}
}

func testRunOrphanAndSpawnFlowFinalPreStopSampleGainingA(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.endpoints = testOrphanEndpoints
	var calls int
	h.deps.Endpoints = func(context.Context) (string, error) {
		calls++
		if calls < 3 {
			return testOrphanEndpoints, nil
		}
		return testOrphanEndpoints + "\naccounts/worker agent ○ idle", nil
	}
	got := runSupervisor(t.Context(), h.deps)
	if got.Reason != "orphan-unstable" || h.hasCall("stop-slice") {
		t.Fatalf("run=%#v calls=%v", got, h.calls)
	}
}

func testRunOrphanAndSpawnFlowHoldAppearingBeforeTheSpawnPrevents(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	probe := h.deps.ProbeZellij
	h.deps.ProbeZellij = func(ctx context.Context) (bool, error) {
		h.write(h.deps.Paths.Hold, fmt.Sprintf(`{"until":%q,"reason":"test"}`, h.time.Add(time.Hour).Format(time.RFC3339Nano)))
		return probe(ctx)
	}
	got := runSupervisor(t.Context(), h.deps)
	state, valid := relaunch.ReadState(h.deps.Paths.State, h.fs)
	if got.Reason != "hold" || !valid || len(state.Spawns) != 0 || h.countPrefix("spawn:") != 0 {
		t.Fatalf("run=%#v state=%#v valid=%t calls=%v", got, state, valid, h.calls)
	}
}
