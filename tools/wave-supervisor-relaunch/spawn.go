package main

import (
	"context"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/cotalreach"
	"github.com/RigelBuild/nebula/tools/go-lib/relaunch"
)

func completeSpawn(ctx context.Context, deps runDeps, state relaunch.RelaunchState, trigger Trigger, supervisorState SupervisorState, manifest *WaveManifest, sessionID string, recoveredWedged bool, startedAt time.Time) Outcome {
	finish := func(outcome Outcome) Outcome { return outcome }
	manifestWritten := manifest != nil
	spawnCode, spawnErr := spawnContinue(ctx, deps, sessionID, trigger, manifestWritten)
	if spawnErr != nil {
		spawnCode = -1
	}
	if spawnCode == cotalreach.SPAWN_REFUSED {
		var outcome *Outcome
		spawnCode, outcome = retrySpawn(ctx, deps, trigger, manifestWritten, supervisorState.Kind, sessionID, startedAt)
		if outcome != nil {
			return *outcome
		}
	}
	if spawnCode == 1 || spawnCode == cotalreach.NOT_REACHABLE {
		return failedSpawnSession(ctx, deps, state, trigger, supervisorState.Kind, sessionID, startedAt, spawnCode)
	}
	if spawnCode == spawnGated {
		return finish(failed("zellij-uncapped", startedAt, deps, supervisorState.Kind, sessionID))
	}
	if spawnCode == 2 {
		return finish(failed("spawn-usage", startedAt, deps, supervisorState.Kind, sessionID))
	}
	if spawnCode != 0 && spawnCode != cotalreach.VIEW_FAILED {
		after, err := classify(ctx, deps)
		if err == nil && after.Kind == stateLive {
			return finish(Outcome{Outcome: outcomeSkipped, Reason: "raced", State: stateLive, ElapsedMS: elapsedMS(deps, startedAt)})
		}
		return finish(failed("spawn-failed", startedAt, deps, supervisorState.Kind, sessionID))
	}
	return finishSpawn(ctx, deps, state, trigger, supervisorState, sessionID, recoveredWedged, startedAt)
}

func failedSpawnSession(ctx context.Context, deps runDeps, state relaunch.RelaunchState, trigger Trigger, supervisorState, sessionID string, startedAt time.Time, spawnCode int) Outcome {
	if spawnCode == cotalreach.NOT_REACHABLE {
		changed, err := managerChanged(ctx, deps, trigger)
		if err != nil || changed {
			return failed("manager-changed", startedAt, deps, supervisorState, sessionID)
		}
		wedged, err := serverWedgedNow(ctx, deps)
		if err != nil || wedged {
			return failed("zellij-wedged", startedAt, deps, supervisorState, sessionID)
		}
	}
	state.Burned = appendUnique(state.Burned, sessionID)
	if err := relaunch.WriteState(deps.Paths.State, state, deps.FS); err != nil {
		return failed("state-unreadable", startedAt, deps, supervisorState, sessionID)
	}
	return failed("no-session", startedAt, deps, supervisorState, sessionID)
}

func retrySpawn(ctx context.Context, deps runDeps, trigger Trigger, manifestWritten bool, state, sessionID string, startedAt time.Time) (int, *Outcome) {
	retry := waitManager(ctx, deps, defaultReadiness(deps))
	if retry.Kind != relaunch.ReadinessReady {
		reason := "manager-not-ready"
		if retry.Kind == relaunch.ReadinessBlocked {
			reason = "manager-blocked"
		}
		return 0, new(failed(reason, startedAt, deps, state, sessionID))
	}
	code, err := spawnContinue(ctx, deps, sessionID, trigger, manifestWritten)
	if err != nil {
		code = -1
	}
	if code == cotalreach.SPAWN_REFUSED {
		return code, new(failed("spawn-refused", startedAt, deps, state, sessionID))
	}
	return code, nil
}

func finishSpawn(ctx context.Context, deps runDeps, state relaunch.RelaunchState, trigger Trigger, supervisorState SupervisorState, sessionID string, recoveredWedged bool, startedAt time.Time) Outcome {
	finish := func(outcome Outcome) Outcome { return outcome }
	currentFingerprint := fingerprint(trigger)
	if !pollPostCondition(ctx, deps) {
		changed, err := managerChanged(ctx, deps, trigger)
		if err == nil && changed {
			return finish(failed("manager-changed", startedAt, deps, supervisorState.Kind, sessionID))
		}
		wedged, err := serverWedgedNow(ctx, deps)
		if err == nil && wedged {
			return finish(failed("zellij-wedged", startedAt, deps, supervisorState.Kind, sessionID))
		}
		state.Burned = appendUnique(state.Burned, sessionID)
		if err := relaunch.WriteState(deps.Paths.State, state, deps.FS); err != nil {
			return finish(failed("state-unreadable", startedAt, deps, supervisorState.Kind, sessionID))
		}
		return finish(failed("not-live", startedAt, deps, supervisorState.Kind, sessionID))
	}
	state.LastFingerprint = currentFingerprint
	if err := relaunch.WriteState(deps.Paths.State, state, deps.FS); err != nil {
		return finish(failed("state-unreadable", startedAt, deps, supervisorState.Kind, sessionID))
	}
	reason := "absent"
	if recoveredWedged {
		reason = "zellij-restarted"
	} else if supervisorState.Kind == stateOrphaned {
		reason = stateOrphaned
	}
	return finish(Outcome{Outcome: "relaunched", Reason: reason, State: supervisorState.Kind, Mode: "continue", Session: sessionID, ElapsedMS: elapsedMS(deps, startedAt)})
}
func appendSpawn(recent []string, stamp string) []string {
	return append(recent, stamp)
}
func spawnContinue(ctx context.Context, deps runDeps, sessionID string, trigger Trigger, manifestWritten bool) (int, error) {
	if uncapped(ctx, deps) {
		return spawnGated, nil
	}
	args := []string{deps.root(), "--continue", sessionID, "--view-session", deps.Config.ZellijSession, "--prompt", relaunchPrompt(deps.Config, trigger, deps.Paths.Manifest, manifestWritten)}
	return deps.Spawn(ctx, args, deps.RootCWD)
}
func pollPostCondition(ctx context.Context, deps runDeps) bool {
	timing := readinessTiming{Deadline: 5 * time.Minute, Poll: 15 * time.Second}
	if deps.Post != nil {
		timing = *deps.Post
	}
	deadline := deps.now().Add(timing.Deadline)
	for {
		ps, err := deps.PS(ctx)
		if err == nil {
			if _, ok := cotalreach.ParsePresentAgentNames(ps)[deps.root()]; ok {
				return true
			}
		}
		if !deps.now().Before(deadline) {
			return false
		}
		delay := timing.Poll
		if remaining := deadline.Sub(deps.now()); delay > remaining {
			delay = remaining
		}
		if err := deps.sleep(ctx, delay); err != nil {
			return false
		}
	}
}
func managerChanged(ctx context.Context, deps runDeps, trigger Trigger) (bool, error) {
	before := ""
	if unit := unitFact(trigger, trigger.ManagerUnit); unit != nil {
		before = unit.InvocationID
	}
	current, err := deps.Trigger(ctx)
	if err != nil {
		return true, err
	}
	unit := unitFact(current, trigger.ManagerUnit)
	return unit == nil || unit.ActiveState != "active" || unit.InvocationID != before, nil
}
func serverWedgedNow(ctx context.Context, deps runDeps) (bool, error) {
	ok, err := deps.ProbeZellij(ctx)
	return !ok, err
}
