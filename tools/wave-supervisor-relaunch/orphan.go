package main

import (
	"context"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/relaunch"
)

func waveStopDeps(deps runDeps) stopWaveDeps {
	return stopWaveDeps{
		CheckSubagents: func(checkCtx context.Context) error {
			return waveLiveSubagents(checkCtx, deps.Paths.Proc, deps.FS, deps.Paths.Sessions, deps.Config.WaveSlice)
		},
		CheckDeadline: time.Duration(seatStopOuterDeadlineMS) * time.Millisecond,
		StopSeats: func(stopCtx context.Context) error {
			ps, err := deps.PS(stopCtx)
			if err != nil {
				return err
			}
			return deps.StopSeats(stopCtx, sortedNames(ParsePsAgentNames(ps)))
		},
		StopSlice: deps.StopSlice,
		PresenceGone: func(presenceCtx context.Context) (bool, error) {
			if deps.ProcScopesPresent() {
				return false, nil
			}
			return deps.EndpointsClear(presenceCtx)
		},
		StopSeatsDeadline: time.Duration(seatStopOuterDeadlineMS) * time.Millisecond,
		Sleep:             deps.Sleep,
		Now:               deps.Now,
		WithTimeout:       context.WithTimeout,
		Deadline:          defaultStop(deps).Deadline,
		Poll:              defaultStop(deps).Poll,
	}
}

type orphanPreparation struct {
	state           relaunch.RelaunchState
	trigger         Trigger
	supervisorState SupervisorState
	manifest        *WaveManifest
	sessionID       string
	charged         bool
	recoveredWedged bool
	outcome         *Outcome
	censusBefore    map[string]struct{}
}

func prepareOrphan(ctx context.Context, deps runDeps, state relaunch.RelaunchState, trigger Trigger, supervisorState SupervisorState, startedAt time.Time) orphanPreparation {
	finish := func(outcome Outcome) orphanPreparation { return orphanPreparation{outcome: new(outcome)} }
	var censusBefore map[string]struct{}
	var manifest *WaveManifest
	var sessionID string
	initial := censusOrphan(ctx, deps, state, trigger, supervisorState, startedAt)
	if initial.outcome != nil {
		return initial
	}
	supervisorState, manifest, sessionID = initial.supervisorState, initial.manifest, initial.sessionID
	censusBefore = initial.censusBefore
	gate := wedgeGate(ctx, deps)
	if gate != wedgeHealthy && gate != wedgeWedged && gate != zellijDown {
		return finish(failed(gate, startedAt, deps, stateOrphaned, sessionID))
	}
	wedged := gate == wedgeWedged
	if gate != wedgeWedged && uncapped(ctx, deps) {
		return finish(failed("zellij-uncapped", startedAt, deps, stateOrphaned, sessionID))
	}
	recent := pruneSpawns(state.Spawns, deps.now())
	if len(recent) >= spawnLimit {
		return finish(failed("budget", startedAt, deps, stateOrphaned, sessionID))
	}
	if holdActive(deps) {
		return finish(Outcome{Outcome: outcomeSkipped, Reason: reasonHold, ElapsedMS: elapsedMS(deps, startedAt)})
	}
	// A subagent refusal must not spend a spawn slot.
	if err := checkWaveSubagents(ctx, waveStopDeps(deps)); err != nil {
		waveStopDeps(deps).noteSubagentRefusal(err)
		return finish(Outcome{Outcome: outcomeSkipped, Reason: "skipped:subagents", State: stateOrphaned, ElapsedMS: elapsedMS(deps, startedAt)})
	}
	if wedged {
		state.Spawns = appendSpawn(recent, isoNow(deps))
		if err := relaunch.WriteState(deps.Paths.State, state, deps.FS); err != nil {
			return finish(failed("state-unreadable", startedAt, deps, stateOrphaned, sessionID))
		}
	}
	latest, outcome := reclassifyOrphan(ctx, deps, supervisorState, startedAt)
	if outcome != nil {
		return finish(*outcome)
	}
	supervisorState = latest
	if !wedged {
		state.Spawns = appendSpawn(recent, isoNow(deps))
		if err := relaunch.WriteState(deps.Paths.State, state, deps.FS); err != nil {
			return finish(failed("state-unreadable", startedAt, deps, stateOrphaned, sessionID))
		}
	}
	result := orphanPreparation{state: state, trigger: trigger, supervisorState: supervisorState, manifest: manifest, sessionID: sessionID, charged: true}
	if outcome := stopOrphan(ctx, deps, manifest, censusBefore, sessionID, startedAt); outcome != nil {
		return finish(*outcome)
	}
	if wedged {
		return recoverOrphan(ctx, deps, result, startedAt)
	}
	return result
}

func recoverOrphan(ctx context.Context, deps runDeps, preparation orphanPreparation, startedAt time.Time) orphanPreparation {
	result := recoverWedge(ctx, deps, preparation.state, preparation.supervisorState, preparation.manifest, startedAt)
	if result.outcome != nil {
		return orphanPreparation{outcome: result.outcome}
	}
	preparation.state = result.state
	preparation.trigger = result.trigger
	preparation.supervisorState = result.supervisorState
	preparation.manifest = result.manifest
	preparation.recoveredWedged = true
	preparation.manifest.At = isoNow(deps)
	preparation.manifest.Fingerprint = fingerprint(result.trigger)
	if err := writeWaveManifest(deps.Paths.Manifest, *preparation.manifest, deps.FS); err != nil {
		return orphanPreparation{outcome: new(failed("manifest-write", startedAt, deps, stateOrphaned, preparation.sessionID))}
	}
	return preparation
}

func stopOrphan(ctx context.Context, deps runDeps, manifest *WaveManifest, censusBefore map[string]struct{}, sessionID string, startedAt time.Time) *Outcome {
	if err := writeWaveManifest(deps.Paths.Manifest, *manifest, deps.FS); err != nil {
		return new(failed("census-failed", startedAt, deps, stateOrphaned, ""))
	}
	stopResult := stopWave(ctx, waveStopDeps(deps))
	if stopResult == "skipped:subagents" {
		return &Outcome{Outcome: outcomeSkipped, Reason: stopResult, State: stateOrphaned, ElapsedMS: elapsedMS(deps, startedAt)}
	}
	if stopResult != "stopped" {
		return new(failed(stopResult, startedAt, deps, stateOrphaned, ""))
	}
	added := reconcileAfter(ctx, deps, censusBefore)
	manifest.At = isoNow(deps)
	manifest.Agents = mergeManifestAgents(manifest.Agents, added)
	if err := writeWaveManifest(deps.Paths.Manifest, *manifest, deps.FS); err != nil {
		return new(failed("manifest-write", startedAt, deps, stateOrphaned, sessionID))
	}
	return nil
}

func reclassifyOrphan(ctx context.Context, deps runDeps, supervisorState SupervisorState, startedAt time.Time) (SupervisorState, *Outcome) {
	latest, err := classify(ctx, deps)
	if err != nil {
		return latest, new(classificationFailed(err, startedAt, deps))
	}
	if latest.Kind == stateInvalid {
		return latest, new(failed("ps-invalid", startedAt, deps, stateOrphaned, ""))
	}
	if latest.Kind == stateStarting {
		return latest, &Outcome{Outcome: outcomeSkipped, Reason: stateStarting, State: stateStarting, ElapsedMS: elapsedMS(deps, startedAt)}
	}
	if latest.Kind != stateOrphaned || !sameNames(supervisorState.OrphanSet, latest.OrphanSet) {
		return latest, &Outcome{Outcome: outcomeSkipped, Reason: "orphan-unstable", State: stateOrphaned, ElapsedMS: elapsedMS(deps, startedAt)}
	}
	return latest, nil
}

func censusOrphan(ctx context.Context, deps runDeps, state relaunch.RelaunchState, trigger Trigger, supervisorState SupervisorState, startedAt time.Time) orphanPreparation {
	finish := func(outcome Outcome) orphanPreparation { return orphanPreparation{outcome: new(outcome)} }
	currentFingerprint := fingerprint(trigger)
	var censusBefore map[string]struct{}
	var manifest *WaveManifest
	var sessionID string
	candidate := append([]string(nil), supervisorState.OrphanSet...)
	if err := waitUntil(ctx, deps, orphanStable); err != nil {
		return finish(failed("orphan-unstable", startedAt, deps, stateOrphaned, ""))
	}
	repeated, err := classify(ctx, deps)
	if err != nil {
		return finish(classificationFailed(err, startedAt, deps))
	}
	if repeated.Kind == stateInvalid {
		return finish(failed("ps-invalid", startedAt, deps, "", ""))
	}
	if repeated.Kind == stateStarting {
		return finish(Outcome{Outcome: outcomeSkipped, Reason: stateStarting, State: stateStarting, ElapsedMS: elapsedMS(deps, startedAt)})
	}
	if repeated.Kind != stateOrphaned || !sameNames(candidate, repeated.OrphanSet) {
		return finish(Outcome{Outcome: outcomeSkipped, Reason: "orphan-unstable", State: stateOrphaned, ElapsedMS: elapsedMS(deps, startedAt)})
	}
	supervisorState = repeated
	censusBefore = relaunch.SnapshotSessionFiles(deps.Paths.Sessions, deps.FS)
	census, err := readWaveManifest(deps.Paths.Proc, deps.FS, deps.Paths.Sessions, deps.Config.WaveSlice)
	if err != nil {
		return finish(failed("census-failed", startedAt, deps, stateOrphaned, ""))
	}
	if census.Unmapped > 0 {
		return finish(failed("census-incomplete", startedAt, deps, stateOrphaned, ""))
	}
	manifest = &WaveManifest{At: isoNow(deps), Fingerprint: currentFingerprint, Agents: census.Agents}
	for _, agent := range manifest.Agents {
		if agent.Name == deps.root() && !contains(state.Burned, agent.SessionID) {
			sessionID = agent.SessionID
		}
	}
	if sessionID == "" {
		sessionID = newestSupervisorSession(deps, state.Burned)
	}
	if sessionID == "" {
		return finish(failed("no-session", startedAt, deps, stateOrphaned, ""))
	}
	return orphanPreparation{supervisorState: supervisorState, manifest: manifest, sessionID: sessionID, censusBefore: censusBefore}
}
