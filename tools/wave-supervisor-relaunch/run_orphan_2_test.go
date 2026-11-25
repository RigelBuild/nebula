package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/cotalreach"
	"github.com/RigelBuild/nebula/tools/go-lib/relaunch"
)

func testRunOrphanAndSpawnFlowUnexpectedNonzeroSpawnRacingToLive(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.spawnCodes = []int{7}
	var calls int
	h.deps.PS = func(context.Context) (string, error) {
		calls++
		if calls == 1 {
			return testPSEnvelope(""), nil
		}
		return testPSEnvelope(testLivePS), nil
	}
	got := runSupervisor(t.Context(), h.deps)
	if got.Reason != "raced" || got.State != "live" {
		t.Fatalf("run=%#v", got)
	}
}

func testRunOrphanAndSpawnFlowUnexpectedNonzeroSpawnRacingToLiveBesideOrphanLane(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.spawnCodes = []int{7}
	var psCalls, endpointCalls int
	h.deps.PS = func(context.Context) (string, error) {
		psCalls++
		if psCalls == 1 {
			return testPSEnvelope(""), nil
		}
		return testPSEnvelope(testLivePS), nil
	}
	h.deps.Endpoints = func(context.Context) (string, error) {
		endpointCalls++
		h.calls = append(h.calls, "endpoints")
		if endpointCalls == 1 {
			return "", nil
		}
		return "accounts/worker agent ○ idle", nil
	}
	got := runSupervisor(t.Context(), h.deps)
	state, valid := relaunch.ReadState(h.deps.Paths.State, h.fs)
	if got.Reason != "raced" || got.State != "live" || !valid || len(state.Burned) != 0 || h.countPrefix("spawn:") != 1 || endpointCalls != 2 {
		t.Fatalf("run=%#v state=%#v valid=%t endpointCalls=%d calls=%v", got, state, valid, endpointCalls, h.calls)
	}
}

func testRunOrphanAndSpawnFlowManagerInvocationChangeAfterARefused(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.spawnCodes = []int{cotalreach.NOT_REACHABLE}
	h.liveAfter = false
	spawned := false
	h.deps.Spawn = func(context.Context, []string, string) (int, error) {
		spawned = true
		return cotalreach.NOT_REACHABLE, nil
	}
	h.deps.ProbeZellij = func(context.Context) (bool, error) { return true, nil }
	h.deps.Trigger = func(context.Context) (Trigger, error) {
		id := "manager"
		if spawned {
			id = "manager-2"
		}
		return Trigger{BootID: "boot", ManagerUnit: "cotal-manager.service", Units: []UnitFact{{Unit: "cotal-manager.service", InvocationID: id, ActiveState: "active"}}, ZellijServer: "500:1"}, nil
	}
	got := runSupervisor(t.Context(), h.deps)
	state, valid := relaunch.ReadState(h.deps.Paths.State, h.fs)
	if got.Reason != "manager-changed" || !valid || len(state.Burned) != 0 {
		t.Fatalf("run=%#v state=%#v valid=%t", got, state, valid)
	}
}

func testRunOrphanAndSpawnFlowManagerInvocationChangeAfterAcceptedSpawn(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.liveAfter = false
	spawned := false
	h.deps.Spawn = func(context.Context, []string, string) (int, error) { spawned = true; return 0, nil }
	h.deps.ProbeZellij = func(context.Context) (bool, error) { return true, nil }
	h.deps.Trigger = func(context.Context) (Trigger, error) {
		id := "manager"
		if spawned {
			id = "manager-2"
		}
		return Trigger{BootID: "boot", ManagerUnit: "cotal-manager.service", Units: []UnitFact{{Unit: "cotal-manager.service", InvocationID: id, ActiveState: "active"}}, ZellijServer: "500:1"}, nil
	}
	got := runSupervisor(t.Context(), h.deps)
	state, valid := relaunch.ReadState(h.deps.Paths.State, h.fs)
	if got.Reason != "manager-changed" || !valid || len(state.Burned) != 0 {
		t.Fatalf("run=%#v state=%#v valid=%t", got, state, valid)
	}
}

func testRunOrphanAndSpawnFlowExit5AfterManagerRestartsDuring(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.liveAfter = false
	spawned := false
	h.deps.Spawn = func(context.Context, []string, string) (int, error) {
		spawned = true
		return cotalreach.NOT_REACHABLE, nil
	}
	h.deps.ProbeZellij = func(context.Context) (bool, error) { return true, nil }
	h.deps.Trigger = func(context.Context) (Trigger, error) {
		id := "manager"
		if spawned {
			id = "manager-2"
		}
		return Trigger{BootID: "boot", ManagerUnit: "cotal-manager.service", Units: []UnitFact{{Unit: "cotal-manager.service", InvocationID: id, ActiveState: "active"}}, ZellijServer: "500:1"}, nil
	}
	got := runSupervisor(t.Context(), h.deps)
	state, valid := relaunch.ReadState(h.deps.Paths.State, h.fs)
	if got.Reason != "manager-changed" || !valid || len(state.Burned) != 0 {
		t.Fatalf("run=%#v state=%#v valid=%t", got, state, valid)
	}
}

func testRunRestartRequestsAndWedgeRecoveryHealthyOrOrphanedSupervisorNeverSpawns(t *testing.T) {
	t.Helper()
	for _, orphaned := range []bool{false, true} {
		name := "absent supervisor"
		h := newRunHarness()
		if orphaned {
			name = "orphaned supervisor"
			h.endpoints = testOrphanEndpoints
		}
		h.unitProcs = "900\n"
		t.Run(name, func(t *testing.T) {
			if got := runSupervisor(t.Context(), h.deps); got.Reason != "zellij-uncapped" || h.countPrefix("spawn:") != 0 || h.hasCall("stop-slice") {
				t.Fatalf("run=%#v calls=%v", got, h.calls)
			}
		})
	}
}

func testRunRestartRequestsAndWedgeRecoveryWedgedOrphanWithStartingSupervisorNotifies(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.endpoints = testOrphanEndpoints
	h.probe = []bool{false, false, false, true}
	restarted := false
	restart := h.deps.RestartZellij
	h.deps.RestartZellij = func(ctx context.Context) (bool, error) { restarted = true; return restart(ctx) }
	polls := 0
	h.deps.PS = func(context.Context) (string, error) {
		if !restarted {
			return testPSEnvelope(""), nil
		}
		polls++
		if polls == 1 {
			return testPSEnvelope(`{"name":"foreman","mesh":"starting","meshView":"current"}`), nil
		}
		return testPSEnvelope(testLivePS), nil
	}
	var notice string
	h.deps.DM = func(_ context.Context, _ string, text string) (bool, error) {
		h.calls = append(h.calls, "dm")
		notice = text
		return true, nil
	}
	got := runSupervisor(t.Context(), h.deps)
	if got.Outcome != "notified" || got.Reason != "zellij-restarted" || h.countPrefix("dm") != 1 || !strings.Contains(notice, "do not spawn them") || h.countPrefix("spawn:") != 0 {
		t.Fatalf("run=%#v notice=%q calls=%v", got, notice, h.calls)
	}
}

func testRunRestartRequestsAndWedgeRecoveryWedgedOrphanEnumeratesProcOnceAnd(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.endpoints = testOrphanEndpoints
	h.probe = []bool{false, false, false, false, true}
	h.installWaveScope()
	procEnumerations := 0
	h.fs.readDirHook = func(path string) ([]fs.DirEntry, error, bool) {
		if path == h.deps.Paths.Proc {
			procEnumerations++
		}
		return nil, nil, false
	}
	if got := runSupervisor(t.Context(), h.deps); got.Reason != "zellij-restarted" {
		t.Fatalf("run = %#v", got)
	}
	state, valid := relaunch.ReadState(h.deps.Paths.State, h.fs)
	if !valid || len(state.Spawns) != 1 {
		t.Fatalf("charged state = %#v, valid=%t", state, valid)
	}
	if procEnumerations != 4 {
		t.Fatalf("proc enumerations = %d, want 4", procEnumerations)
	}
}

func testRunRestartRequestsAndWedgeRecoveryWedgedOrphanUsesThirdSpawnBudget(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.endpoints = testOrphanEndpoints
	h.probe = []bool{false, false, false, true, true}
	h.installWaveScope()
	stamp := h.time.Add(-time.Minute).Format(time.RFC3339Nano)
	h.write(h.deps.Paths.State, fmt.Sprintf(`{"version":1,"spawns":[%q,%q],"burned":[]}`, stamp, stamp))
	got := runSupervisor(t.Context(), h.deps)
	state, valid := relaunch.ReadState(h.deps.Paths.State, h.fs)
	if got.Outcome != "relaunched" || got.Reason != "zellij-restarted" || !valid || len(state.Spawns) != 3 {
		t.Fatalf("run=%#v state=%#v valid=%t", got, state, valid)
	}
	h.requireSingleSpawnPrefix(t, "spawn:foreman --continue census-supervisor --view-session viewer --prompt")
}

func testRunRestartRequestsAndWedgeRecoveryWedgedOrphanRestartCannotUseUncapped(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.endpoints = testOrphanEndpoints
	h.probe = []bool{false, false, false, true}
	h.installWaveScope()
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
	if got.Reason != "zellij-restart-uncapped" || !h.hasCall("restart") || h.countPrefix("spawn:") != 0 {
		t.Fatalf("run=%#v calls=%v", got, h.calls)
	}
}

func testRunOrphanAndSpawnFlowStableSupervisorLanesDoNotStop(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.ps = testLivePS
	h.endpoints = "accounts/worker agent ○ idle"
	runSupervisor(t.Context(), h.deps)
	if h.hasCall("stop-slice") {
		t.Fatalf("unexpected stop: %v", h.calls)
	}
}

func testRunOrphanAndSpawnFlowStartingSupervisorDoesNotStopOr(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.ps = `{"name":"foreman","mesh":"starting"}`
	h.endpoints = "accounts/worker agent ○ idle"
	got := runSupervisor(t.Context(), h.deps)
	if got.Reason != "starting" || h.hasCall("stop-slice") || h.countPrefix("spawn:") != 0 {
		t.Fatalf("run=%#v calls=%v", got, h.calls)
	}
}

func testRunOrphanAndSpawnFlowMissingSessionFailsWithoutSpawning(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.fs.files = map[string][]byte{}
	got := runSupervisor(t.Context(), h.deps)
	if got.Reason != "no-session" || h.countPrefix("spawn:") != 0 {
		t.Fatalf("run=%#v calls=%v", got, h.calls)
	}
}

func testRunOrphanAndSpawnFlowMalformedInitialPsFailsWithoutStopping(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.endpoints = testOrphanEndpoints
	h.ps = "not json"
	if got := runSupervisor(t.Context(), h.deps); got.Reason != "ps-invalid" || h.hasCall("stop-slice") {
		t.Fatalf("run=%#v calls=%v", got, h.calls)
	}
}

func testRunOrphanAndSpawnFlowManagerNemoAndALaneOnly(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.endpoints = "manager/manager endpoint ○ idle\nnemo/nemo agent ● working\naccounts/worker agent ○ idle"
	h.ps = `{"name":"accounts","mesh":"starting"}`
	if got := runSupervisor(t.Context(), h.deps); got.Outcome != "relaunched" || h.hasCall("stop-slice") {
		t.Fatalf("run=%#v calls=%v", got, h.calls)
	}
}

func testRunOrphanAndSpawnFlowASpawnProceedsWithDevboxDown(t *testing.T) {
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

func testRunOrphanAndSpawnFlowWedgedServerAfterFailedPostconditionIs(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.liveAfter = false
	h.probe = []bool{true, false}
	var spawnCalls int
	h.deps.Spawn = func(context.Context, []string, string) (int, error) { spawnCalls++; return 0, nil }
	got := runSupervisor(t.Context(), h.deps)
	state, valid := relaunch.ReadState(h.deps.Paths.State, h.fs)
	if got.Reason != "zellij-wedged" || !valid || len(state.Burned) != 0 || spawnCalls != 1 || h.hasCall("restart") {
		t.Fatalf("run=%#v state=%#v valid=%t spawnCalls=%d calls=%v", got, state, valid, spawnCalls, h.calls)
	}
}

func testRunOrphanAndSpawnFlowSpawnBudgetCapsAttempts(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	stamp := h.time.Format(time.RFC3339Nano)
	h.write(h.deps.Paths.State, fmt.Sprintf(`{"version":1,"spawns":[%q,%q,%q],"burned":[]}`, stamp, stamp, stamp))
	if got := runSupervisor(t.Context(), h.deps); got.Reason != "budget" || h.countPrefix("spawn:") != 0 {
		t.Fatalf("run=%#v calls=%v", got, h.calls)
	}
}

func testRunOrphanAndSpawnFlowFailedSliceStopReturnsWaveStop(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.endpoints = testOrphanEndpoints
	h.deps.StopSlice = func(context.Context) (bool, error) { return false, nil }
	if got := runSupervisor(t.Context(), h.deps); got.Reason != "wave-stop" || h.countPrefix("spawn:") != 0 {
		t.Fatalf("run=%#v calls=%v", got, h.calls)
	}
}

func testRunOrphanAndSpawnFlowOrphanStillPresentThroughStopDeadline(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.endpoints = testOrphanEndpoints
	h.deps.ProcScopesPresent = func() bool { return true }
	h.deps.Stop = &stopTiming{Deadline: 2 * time.Millisecond, Poll: time.Millisecond}
	if got := runSupervisor(t.Context(), h.deps); got.Reason != "presence-stuck" {
		t.Fatalf("run=%#v", got)
	}
}

func testRunOrphanAndSpawnFlowSpawnRetrySeesUncappedServerAnd(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.spawnCodes = []int{cotalreach.SPAWN_REFUSED, 0}
	calls := 0
	h.deps.Spawn = func(context.Context, []string, string) (int, error) {
		calls++
		h.unitProcs = "900\n"
		return cotalreach.SPAWN_REFUSED, nil
	}
	got := runSupervisor(t.Context(), h.deps)
	if got.Reason != "zellij-uncapped" || calls != 1 {
		t.Fatalf("run=%#v spawn calls=%d", got, calls)
	}
}

func testRunOrphanAndSpawnFlowOrphanStabilitySleepCancellationFailsClosed(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.endpoints = testOrphanEndpoints
	cancelled := false
	h.deps.Sleep = func(context.Context, time.Duration) error { cancelled = true; return context.Canceled }
	if got := runSupervisor(t.Context(), h.deps); got.Reason != "orphan-unstable" || !cancelled || h.hasCall("stop-slice") {
		t.Fatalf("run=%#v calls=%v cancelled=%t", got, h.calls, cancelled)
	}
}

func testRunOrphanAndSpawnFlowOrphanWithMissingSessionFailsBefore(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.endpoints = testOrphanEndpoints
	h.fs.files = map[string][]byte{}
	if got := runSupervisor(t.Context(), h.deps); got.Reason != "no-session" || h.hasCall("stop-slice") {
		t.Fatalf("run=%#v calls=%v", got, h.calls)
	}
}

func testRunOrphanAndSpawnFlowOrphanCensusUnmappedProcessFailsClosed(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.endpoints = testOrphanEndpoints
	h.fs.files["/proc/126/cgroup"] = []byte("0::/wave-agents.slice/scope")
	h.fs.files["/proc/126/cmdline"] = []byte("omp\x00")
	if got := runSupervisor(t.Context(), h.deps); got.Reason != "census-incomplete" || h.hasCall("stop-slice") {
		t.Fatalf("run=%#v calls=%v", got, h.calls)
	}
}

func testRunOrphanAndSpawnFlowStopRunnerFailureIsBestEffort(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.endpoints = testOrphanEndpoints
	h.deps.StopSeats = func(context.Context, []string) error { return errors.New("seat stop failed") }
	if got := runSupervisor(t.Context(), h.deps); got.Outcome != "relaunched" || !h.hasCall("stop-slice") {
		t.Fatalf("run=%#v calls=%v", got, h.calls)
	}
}

func testRunOrphanAndSpawnFlowManagerSeatStopsPrecedeTheSlice(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.endpoints = testOrphanEndpoints
	h.ps = `{"name":"accounts","mesh":"idle","meshView":"current"}`
	if got := runSupervisor(t.Context(), h.deps); got.Outcome != "relaunched" {
		t.Fatalf("run=%#v", got)
	}
	seatStop, sliceStop := -1, -1
	for index, call := range h.calls {
		if strings.HasPrefix(call, "cotal stop --name") {
			seatStop = index
		}
		if call == "stop-slice" {
			sliceStop = index
		}
	}
	if seatStop < 0 || sliceStop < 0 || seatStop >= sliceStop {
		t.Fatalf("stop order=%v", h.calls)
	}
}

// TestOrphanSubagentGate exercises the whole-wave stop through runSupervisor using fake process
// and session files.
func TestOrphanSubagentGate(t *testing.T) {
	t.Run("a dead seat counts zero and recovery proceeds", func(t *testing.T) {
		h := newRunHarness()
		h.endpoints = testOrphanEndpoints
		h.ps = `{"name":"accounts","mesh":"idle","meshView":"current"}`
		h.deps.StopSeats = func(ctx context.Context, names []string) error {
			return stopManagedSeats(ctx, names, h.deps.Config.CotalSpace, func(_ context.Context, argv []string, _ string) (cotalreach.CommandResult, error) {
				h.calls = append(h.calls, strings.Join(argv, " "))
				return cotalreach.CommandResult{}, nil
			}, liveCheckFor(h.deps.Paths.Proc, h.deps.FS, h.deps.Paths.Sessions, h.deps.Config.WaveSlice))
		}
		if got := runSupervisor(t.Context(), h.deps); got.Outcome != "relaunched" || !h.hasCall("stop-slice") || !h.hasCall("cotal stop --name accounts --space main") {
			t.Fatalf("run=%#v calls=%v", got, h.calls)
		}
	})
	t.Run("a live subagent skips and stops nothing", func(t *testing.T) {
		h := newRunHarness()
		h.endpoints = testOrphanEndpoints
		h.installWaveScope()
		h.fs.files["/sessions/-home-example-agents/session_census-supervisor/Sub.jsonl"] = []byte(`{"type":"model_usage"}` + "\n")
		got := runSupervisor(t.Context(), h.deps)
		if got.Outcome != outcomeSkipped || got.Reason != "skipped:subagents" || h.hasCall("stop-slice") || h.countPrefix("cotal stop") != 0 {
			t.Fatalf("run=%#v calls=%v", got, h.calls)
		}
		if state, _ := relaunch.ReadState(h.deps.Paths.State, h.fs); len(state.Spawns) != 0 {
			t.Fatalf("spawns after refusal = %q, want none", state.Spawns)
		}
	})
	t.Run("one unreadable seat stops no seat and no slice", func(t *testing.T) {
		h := newRunHarness()
		h.endpoints = testOrphanEndpoints
		h.installWaveScope()
		h.fs.files["/proc/112/cgroup"] = []byte("0::/wave-agents.slice/scope2")
		h.fs.files["/proc/112/cmdline"] = []byte("omp\x00")
		h.fs.files["/proc/112/environ"] = []byte("COTAL_AGENT_FILE=/agents/accounts.md\x00")
		h.fs.readHook = func(path string) ([]byte, error, bool) {
			if path == "/proc/112/environ" {
				return nil, errors.New("permission denied"), true
			}
			return nil, nil, false
		}
		got := runSupervisor(t.Context(), h.deps)
		if got.Outcome != outcomeFailed || got.Reason != "census-incomplete" || h.hasCall("stop-slice") || h.countPrefix("cotal stop") != 0 {
			t.Fatalf("run=%#v calls=%v", got, h.calls)
		}
		if state, _ := relaunch.ReadState(h.deps.Paths.State, h.fs); len(state.Spawns) != 0 {
			t.Fatalf("spawns = %q, want none", state.Spawns)
		}
	})
}
