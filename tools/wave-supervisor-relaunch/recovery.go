package main

import (
	"context"
	"fmt"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/relaunch"
	"github.com/RigelBuild/nebula/tools/go-lib/waveseats"
)

func wedgeGate(ctx context.Context, deps runDeps) string {
	verdict := checkWedge(ctx, WedgeDeps{Probe: func() (bool, error) { return deps.ProbeZellij(ctx) }, Identity: deps.ZellijIdentity, Sleep: func(ms int64) error { return deps.sleep(ctx, time.Duration(ms)*time.Millisecond) }})
	switch verdict.Kind {
	case wedgeHealthy:
		return wedgeHealthy
	case "down":
		return zellijDown
	case wedgeUnverified:
		return "zellij-wedge-unverified"
	case wedgeWedged:
		if ownedByUnit(verdict.Identity, callText(ctx, deps.ZellijUnitProcs)) {
			return wedgeWedged
		}
		return "zellij-wedged-outside-unit"
	default:
		return "zellij-wedge-unverified"
	}
}

type wedgeRecovery struct {
	state           relaunch.RelaunchState
	trigger         Trigger
	supervisorState SupervisorState
	manifest        *WaveManifest
	outcome         *Outcome
}

func recoverWedge(ctx context.Context, deps runDeps, state relaunch.RelaunchState, supervisorState SupervisorState, manifest *WaveManifest, startedAt time.Time) wedgeRecovery {
	ready := waitManager(ctx, deps, readinessTiming{Deadline: 2 * time.Minute, Poll: defaultReadiness(deps).Poll})
	if ready.Kind != relaunch.ReadinessReady {
		return wedgeRecovery{outcome: new(failed("manager-recovering", startedAt, deps, supervisorState.Kind, ""))}
	}
	recovered := restartWedged(ctx, deps, startedAt)
	if recovered.trigger != nil {
		state.LastFingerprint = fingerprint(*recovered.trigger)
		if err := relaunch.WriteState(deps.Paths.State, state, deps.FS); err != nil {
			return wedgeRecovery{outcome: new(failed("state-unreadable", startedAt, deps, supervisorState.Kind, ""))}
		}
	}
	if recovered.outcome != nil {
		return wedgeRecovery{outcome: recovered.outcome}
	}
	trigger := *recovered.trigger
	manifest.Fingerprint = fingerprint(trigger)
	manifest.At = isoNow(deps)
	if err := writeWaveManifest(deps.Paths.Manifest, *manifest, deps.FS); err != nil {
		return wedgeRecovery{outcome: new(failed("manifest-write", startedAt, deps, supervisorState.Kind, ""))}
	}
	ready = waitManager(ctx, deps, readinessTiming{Deadline: 2 * time.Minute, Poll: defaultReadiness(deps).Poll})
	if ready.Kind != relaunch.ReadinessReady {
		return wedgeRecovery{outcome: new(failed("manager-recovering", startedAt, deps, supervisorState.Kind, ""))}
	}
	stateAfter, err := classify(ctx, deps)
	if err != nil {
		return wedgeRecovery{outcome: new(classificationFailed(err, startedAt, deps))}
	}
	if stateAfter.Kind == stateInvalid {
		return wedgeRecovery{outcome: new(failed("ps-invalid", startedAt, deps, "", ""))}
	}
	if stateAfter.Kind == stateLive {
		return wedgeRecovery{outcome: new(Outcome{Outcome: outcomeSkipped, Reason: "raced", State: stateLive, ElapsedMS: elapsedMS(deps, startedAt)})}
	}
	if stateAfter.Kind == stateStarting {
		if !pollPostCondition(ctx, deps) {
			return wedgeRecovery{outcome: new(failed("zellij-restart-failed", startedAt, deps, stateStarting, ""))}
		}
		sent, err := deps.DM(ctx, deps.root(), fmt.Sprintf("Zellij restarted. Lanes relaunch from %s; do not spawn them. The stop is recorded in %s.", waveseats.SeatsPath(), deps.Paths.Manifest))
		if err != nil || !sent {
			return wedgeRecovery{outcome: new(failed("notice-failed", startedAt, deps, stateLive, ""))}
		}
		state.LastFingerprint, state.LastNoticeAt = fingerprint(trigger), isoNow(deps)
		if err := relaunch.WriteState(deps.Paths.State, state, deps.FS); err != nil {
			return wedgeRecovery{outcome: new(failed("state-unreadable", startedAt, deps, stateLive, ""))}
		}
		return wedgeRecovery{outcome: new(Outcome{Outcome: "notified", Reason: "zellij-restarted", State: stateLive, ElapsedMS: elapsedMS(deps, startedAt)})}
	}
	return wedgeRecovery{state: state, trigger: trigger, supervisorState: stateAfter, manifest: manifest}
}

func restartViewer(ctx context.Context, deps runDeps, start time.Time, request *relaunch.RestartRequest) Outcome {
	if holdActive(deps) {
		return Outcome{Outcome: outcomeSkipped, Reason: reasonHold, ElapsedMS: elapsedMS(deps, start)}
	}
	if request != nil && request.Cause == restartRSS && deps.ZellijIdentity(ctx) != request.Identity {
		return Outcome{Outcome: outcomeSkipped, Reason: restartNotNeeded, ElapsedMS: elapsedMS(deps, start)}
	}
	restarted, err := deps.RestartZellij(ctx)
	if err != nil || !restarted {
		return failed("zellij-restart-failed", start, deps, "", "")
	}
	deadline := deps.now().Add(2 * time.Minute)
	for deps.now().Before(deadline) {
		text, err := deps.ZellijSessions(ctx)
		if err == nil && relaunch.DevboxLive(text) {
			ok, err := deps.ProbeZellij(ctx)
			if err == nil && ok {
				return finishViewerRestart(ctx, deps, start)
			}
		}
		if err := deps.sleep(ctx, 5*time.Second); err != nil {
			return failed("zellij-restart-failed", start, deps, "", "")
		}
	}
	return failed("zellij-restart-failed", start, deps, "", "")
}

func finishViewerRestart(ctx context.Context, deps runDeps, start time.Time) Outcome {
	trigger, err := deps.Trigger(ctx)
	if err != nil {
		return failed("trigger-unavailable", start, deps, "", "")
	}
	state, valid := relaunch.ReadState(deps.Paths.State, deps.FS)
	if valid {
		state.LastFingerprint = fingerprint(trigger)
		if err := relaunch.WriteState(deps.Paths.State, state, deps.FS); err != nil {
			return failed("state-unreadable", start, deps, "", "")
		}
	}
	if !ownedByUnit(trigger.ZellijServer, callText(ctx, deps.ZellijUnitProcs)) {
		return failed("zellij-restart-uncapped", start, deps, "", "")
	}
	return Outcome{Outcome: outcomeSkipped, Reason: "viewer-restarted", ElapsedMS: elapsedMS(deps, start)}
}
func restartWedged(ctx context.Context, deps runDeps, start time.Time) recoveryResult {
	if holdActive(deps) {
		return recoveryResult{outcome: &Outcome{Outcome: outcomeSkipped, Reason: reasonHold, ElapsedMS: elapsedMS(deps, start)}}
	}
	restarted, err := deps.RestartZellij(ctx)
	if err != nil || !restarted {
		return recoveryResult{outcome: new(failed("zellij-restart-failed", start, deps, "", ""))}
	}
	deadline := deps.now().Add(2 * time.Minute)
	for deps.now().Before(deadline) {
		text, err := deps.ZellijSessions(ctx)
		if err == nil && relaunch.DevboxLive(text) {
			ok, err := deps.ProbeZellij(ctx)
			if err == nil && ok {
				return recoveredTrigger(ctx, deps, start)
			}
		}
		if err := deps.sleep(ctx, 5*time.Second); err != nil {
			break
		}
	}
	return recoveryResult{outcome: new(failed("zellij-restart-failed", start, deps, "", ""))}
}

func recoveredTrigger(ctx context.Context, deps runDeps, start time.Time) recoveryResult {
	trigger, err := deps.Trigger(ctx)
	if err != nil {
		return recoveryResult{outcome: new(failed("trigger-unavailable", start, deps, "", ""))}
	}
	if !ownedByUnit(trigger.ZellijServer, callText(ctx, deps.ZellijUnitProcs)) {
		return recoveryResult{trigger: &trigger, outcome: new(failed("zellij-restart-uncapped", start, deps, "", ""))}
	}
	return recoveryResult{trigger: &trigger}
}

type recoveryResult struct {
	trigger *Trigger
	outcome *Outcome
}
