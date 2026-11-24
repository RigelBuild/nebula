package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/cotalreach"
	"github.com/RigelBuild/nebula/tools/go-lib/relaunch"
	"github.com/RigelBuild/nebula/tools/go-lib/waveseats"
)

const (
	spawnWindow         = time.Hour
	noticeWindow        = 15 * time.Minute
	spawnLimit          = 3
	orphanStable        = 30 * time.Second
	spawnGated          = -2
	outcomeFailed       = "failed"
	outcomeSkipped      = "skipped"
	reasonHold          = "hold"
	restartRSS          = "rss"
	identityNone        = "none"
	identityUnavailable = "unavailable"
	restartNotNeeded    = "restart-not-needed"
	wedgeHealthy        = "healthy"
	wedgeWedged         = "wedged"
	stateInvalid        = "invalid"
	stateOrphaned       = "orphaned"
	stateStarting       = "starting"
	zellijDown          = "zellij-down"
	stateLive           = "live"
	cotalCommand        = "cotal"
	spaceFlag           = "--space"
	userFlag            = "--user"
	zellijCommand       = "zellij"
	wedgeUnverified     = "unverified"
	factUnknown         = "unknown"
	systemctlCommand    = "systemctl"
)

var identityRE = regexp.MustCompile(`^[0-9]+:[0-9]+$`)

type toolPaths struct {
	State          string
	Hold           string
	Manifest       string
	RestartRequest string
	Sessions       string
	Home           string
	Proc           string
	Seats          string
	Cgroup         string
}

type processRunner func(context.Context, []string, string) (cotalreach.CommandResult, error)

type runDeps struct {
	Config            Config
	FS                relaunch.FileSystem
	Root              string
	RootCWD           string
	Paths             toolPaths
	Now               func() time.Time
	Sleep             func(context.Context, time.Duration) error
	Status            func(context.Context) (any, error)
	PS                func(context.Context) (string, error)
	Endpoints         func(context.Context) (string, error)
	Trigger           func(context.Context) (Trigger, error)
	StopSlice         func(context.Context) (bool, error)
	StopSeats         func(context.Context, []string) error
	ProcScopesPresent func() bool
	SeatNames         func(context.Context) (map[string]struct{}, error)
	ReadRootCWD       func(context.Context) (string, error)
	EndpointsClear    func(context.Context) (bool, error)
	ZellijSessions    func(context.Context) (string, error)
	ProbeZellij       func(context.Context) (bool, error)
	ZellijIdentity    func(context.Context) string
	ZellijUnitProcs   func(context.Context) (string, error)
	RestartZellij     func(context.Context) (bool, error)
	StartRelaunch     func(context.Context) (bool, error)
	Spawn             func(context.Context, []string, string) (int, error)
	DM                func(context.Context, string, string) (bool, error)
	Holders           func(context.Context) (string, error)
	Runner            processRunner
	SeatThreshold     string
	Log               func(string)
	WriteLog          func(string) error
	Readiness         *readinessTiming
	Stop              *stopTiming
	Post              *readinessTiming
	ServiceResult     string
	ServiceExitStatus string
}

type readinessTiming struct {
	Deadline time.Duration
	Poll     time.Duration
}

type stopTiming struct {
	Deadline time.Duration
	Poll     time.Duration
}

type envGetter func(string) string

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func cotalFailure(args []string, result cotalreach.CommandResult, elapsed time.Duration) string {
	command := "command"
	if len(args) > 0 {
		command = args[0]
	}
	first := ""
	for line := range strings.SplitSeq(result.Stderr, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			first = line
			break
		}
	}
	suffix := ""
	if len(first) > 120 {
		first = first[:120]
	}
	if first != "" {
		suffix = " stderr=" + first
	}
	return fmt.Sprintf("cotal %s exit=%d ms=%d%s", command, result.ExitCode, elapsed.Milliseconds(), suffix)
}

func runSupervisor(ctx context.Context, deps runDeps) Outcome {
	startedAt := deps.now()
	finish := func(outcome Outcome) Outcome {
		return outcome
	}
	request, staleRequest := readRestartRequest(deps)
	if holdActive(deps) {
		releaseHeldRequest(deps, request, staleRequest)
		return Outcome{Outcome: outcomeSkipped, Reason: reasonHold, ElapsedMS: elapsedMS(deps, startedAt)}
	}
	names, err := deps.SeatNames(ctx)
	if err != nil {
		return classificationFailed(err, startedAt, deps)
	}
	deps.Root = activeRootNames(names)
	if deps.Root == "" {
		return Outcome{Outcome: outcomeSkipped, Reason: "root-seat-inactive", ElapsedMS: elapsedMS(deps, startedAt)}
	}
	rootCWD, err := deps.ReadRootCWD(ctx)
	if err != nil {
		return classificationFailed(err, startedAt, deps)
	}
	if rootCWD == "" {
		return Outcome{Outcome: outcomeSkipped, Reason: "root-cwd-missing", ElapsedMS: elapsedMS(deps, startedAt)}
	}
	deps.RootCWD = rootCWD
	state, outcome := prepareRunRequest(ctx, deps, startedAt, request, staleRequest)
	if outcome != nil {
		return *outcome
	}

	readiness := waitManager(ctx, deps, defaultReadiness(deps))
	if readiness.Kind != relaunch.ReadinessReady {
		reason := "manager-not-ready"
		if readiness.Kind == relaunch.ReadinessBlocked {
			reason = "manager-blocked"
		}
		return finish(failed(reason, startedAt, deps, "", ""))
	}
	trigger, err := deps.Trigger(ctx)
	if err != nil {
		return finish(classificationFailed(err, startedAt, deps))
	}
	currentFingerprint := fingerprint(trigger)
	if fact := unitFact(trigger, trigger.ManagerUnit); fact != nil && fact.ActiveState != "active" {
		return finish(Outcome{Outcome: outcomeSkipped, Reason: "manager-stopping", ElapsedMS: elapsedMS(deps, startedAt)})
	}
	supervisorState, err := classify(ctx, deps)
	if err != nil {
		return finish(classificationFailed(err, startedAt, deps))
	}
	if supervisorState.Kind == stateInvalid {
		return finish(failed("ps-invalid", startedAt, deps, "", ""))
	}
	var manifest *WaveManifest
	var sessionID string
	charged := false
	recoveredWedged := false
	if supervisorState.Kind == stateOrphaned {
		result := prepareOrphan(ctx, deps, state, trigger, supervisorState, startedAt)
		if result.outcome != nil {
			return *result.outcome
		}
		state, trigger, supervisorState = result.state, result.trigger, result.supervisorState
		manifest, sessionID = result.manifest, result.sessionID
		charged, recoveredWedged = result.charged, result.recoveredWedged
		currentFingerprint = fingerprint(trigger)
	}
	if supervisorState.Kind == stateLive {
		return notifyLiveSupervisor(ctx, deps, state, trigger, currentFingerprint, startedAt)
	}
	if supervisorState.Kind == stateStarting {
		return finish(Outcome{Outcome: outcomeSkipped, Reason: stateStarting, State: stateStarting, ElapsedMS: elapsedMS(deps, startedAt)})
	}
	return launchSupervisor(ctx, deps, state, trigger, supervisorState, manifest, sessionID, charged, recoveredWedged, startedAt)
}

func prepareRunRequest(ctx context.Context, deps runDeps, startedAt time.Time, request *relaunch.RestartRequest, staleRequest bool) (relaunch.RelaunchState, *Outcome) {
	state, valid := relaunch.ReadState(deps.Paths.State, deps.FS)
	if !valid {
		return state, new(failed("state-unreadable", startedAt, deps, "", ""))
	}
	if request != nil || staleRequest {
		if err := relaunch.ReleaseRestartRequest(deps.Paths.RestartRequest, deps.FS); err != nil {
			deps.warn("release restart request", err)
		}
	}
	return state, requestedRestart(ctx, deps, startedAt, request, staleRequest)
}

func releaseHeldRequest(deps runDeps, request *relaunch.RestartRequest, stale bool) {
	if request != nil && !stale && request.Cause == restartRSS {
		if err := relaunch.ReleaseRestartRequest(deps.Paths.RestartRequest, deps.FS); err != nil {
			deps.warn("release gated RSS restart request", err)
		}
	}
}

func launchSupervisor(ctx context.Context, deps runDeps, state relaunch.RelaunchState, trigger Trigger, supervisorState SupervisorState, manifest *WaveManifest, sessionID string, charged, recoveredWedged bool, startedAt time.Time) Outcome {
	finish := func(outcome Outcome) Outcome { return outcome }
	if sessionID == "" {
		sessionID = newestSupervisorSession(deps, state.Burned)
	}
	if sessionID == "" || contains(state.Burned, sessionID) {
		return finish(failed("no-session", startedAt, deps, supervisorState.Kind, ""))
	}
	if !recoveredWedged && supervisorState.Kind != stateOrphaned {
		var outcome *Outcome
		trigger, outcome = launchWedgeGate(ctx, deps, trigger, supervisorState.Kind, sessionID, startedAt)
		if outcome != nil {
			return *outcome
		}
	}
	if uncapped(ctx, deps) {
		return finish(failed("zellij-uncapped", startedAt, deps, supervisorState.Kind, sessionID))
	}
	recent := pruneSpawns(state.Spawns, deps.now())
	if !charged && len(recent) >= spawnLimit {
		return finish(failed("budget", startedAt, deps, supervisorState.Kind, sessionID))
	}
	if !charged && holdActive(deps) {
		return finish(Outcome{Outcome: outcomeSkipped, Reason: reasonHold, ElapsedMS: elapsedMS(deps, startedAt)})
	}
	if !charged {
		state.Spawns = appendSpawn(recent, isoNow(deps))
		if err := relaunch.WriteState(deps.Paths.State, state, deps.FS); err != nil {
			return finish(failed("state-unreadable", startedAt, deps, supervisorState.Kind, sessionID))
		}
	}
	return completeSpawn(ctx, deps, state, trigger, supervisorState, manifest, sessionID, recoveredWedged, startedAt)
}

func launchWedgeGate(ctx context.Context, deps runDeps, trigger Trigger, state, sessionID string, startedAt time.Time) (Trigger, *Outcome) {
	gate := wedgeGate(ctx, deps)
	if gate != wedgeHealthy && gate != wedgeWedged && gate != zellijDown {
		return trigger, new(failed(gate, startedAt, deps, state, sessionID))
	}
	if gate == wedgeWedged {
		viewer := restartViewer(ctx, deps, startedAt, nil)
		if viewer.Reason != "viewer-restarted" {
			return trigger, &viewer
		}
		var err error
		trigger, err = deps.Trigger(ctx)
		if err != nil {
			return trigger, new(classificationFailed(err, startedAt, deps))
		}
	}
	return trigger, nil
}

func requestedRestart(ctx context.Context, deps runDeps, startedAt time.Time, request *relaunch.RestartRequest, staleRequest bool) *Outcome {
	finish := func(outcome Outcome) *Outcome { return new(outcome) }
	requestedWedge := false
	if request != nil && !staleRequest {
		if request.Cause == restartRSS {
			identity := deps.ZellijIdentity(ctx)
			switch {
			case identity == identityNone:
				return finish(failed(zellijDown, startedAt, deps, "", ""))
			case identity == identityUnavailable:
				return finish(failed("zellij-wedge-unverified", startedAt, deps, "", ""))
			case identity != request.Identity:
				return finish(Outcome{Outcome: outcomeSkipped, Reason: restartNotNeeded, ElapsedMS: elapsedMS(deps, startedAt)})
			case !ownedByUnit(identity, callText(ctx, deps.ZellijUnitProcs)):
				return finish(failed("zellij-wedged-outside-unit", startedAt, deps, "", ""))
			default:
				requestedWedge = true
			}
		} else {
			gate := wedgeGate(ctx, deps)
			if gate == wedgeHealthy {
				return finish(Outcome{Outcome: outcomeSkipped, Reason: restartNotNeeded, ElapsedMS: elapsedMS(deps, startedAt)})
			}
			if gate != wedgeWedged {
				return finish(failed(gate, startedAt, deps, "", ""))
			}
			requestedWedge = true
		}
	}
	if requestedWedge {
		return finish(restartViewer(ctx, deps, startedAt, request))
	}
	return nil
}

func notifyLiveSupervisor(ctx context.Context, deps runDeps, state relaunch.RelaunchState, trigger Trigger, currentFingerprint string, startedAt time.Time) Outcome {
	finish := func(outcome Outcome) Outcome { return outcome }
	if trigger.ZellijServer == identityUnavailable && strings.HasPrefix(state.LastFingerprint, legacyPrefix(trigger)+":") {
		return finish(Outcome{Outcome: outcomeSkipped, Reason: "devbox-unavailable", State: stateLive, ElapsedMS: elapsedMS(deps, startedAt)})
	}
	if trigger.ZellijServer != identityNone && state.LastFingerprint != "" && (!strings.Contains(state.LastFingerprint, ":devbox=") || strings.HasSuffix(state.LastFingerprint, ":devbox=unavailable")) && strings.HasPrefix(state.LastFingerprint, legacyPrefix(trigger)+":") {
		state.LastFingerprint = currentFingerprint
		if err := relaunch.WriteState(deps.Paths.State, state, deps.FS); err != nil {
			return finish(failed("state-unreadable", startedAt, deps, stateLive, ""))
		}
		return finish(Outcome{Outcome: outcomeSkipped, Reason: "rebaselined", State: stateLive, ElapsedMS: elapsedMS(deps, startedAt)})
	}
	if state.LastFingerprint == currentFingerprint {
		return finish(Outcome{Outcome: outcomeSkipped, Reason: "unchanged", State: stateLive, ElapsedMS: elapsedMS(deps, startedAt)})
	}
	lastFingerprint := state.LastFingerprint
	if parsed, err := time.Parse(time.RFC3339Nano, state.LastNoticeAt); err == nil && deps.now().Sub(parsed) < noticeWindow {
		return finish(Outcome{Outcome: outcomeSkipped, Reason: "notice-rate", State: stateLive, ElapsedMS: elapsedMS(deps, startedAt)})
	}
	text := armedNotice(trigger)
	if lastFingerprint != "" {
		text = liveNotice(trigger)
	}
	sent, err := deps.DM(ctx, deps.root(), text)
	if err != nil || !sent {
		return finish(failed("notice-failed", startedAt, deps, stateLive, ""))
	}
	state.LastFingerprint, state.LastNoticeAt = currentFingerprint, isoNow(deps)
	if err := relaunch.WriteState(deps.Paths.State, state, deps.FS); err != nil {
		return finish(failed("state-unreadable", startedAt, deps, stateLive, ""))
	}
	reason := "trigger-changed"
	if lastFingerprint == "" {
		reason = "armed"
	}
	return finish(Outcome{Outcome: "notified", Reason: reason, State: stateLive, ElapsedMS: elapsedMS(deps, startedAt)})
}

func defaultReadiness(deps runDeps) readinessTiming {
	if deps.Readiness != nil {
		return *deps.Readiness
	}
	return readinessTiming{Deadline: 5 * time.Minute, Poll: 2 * time.Second}
}
func defaultStop(deps runDeps) stopTiming {
	if deps.Stop != nil {
		return *deps.Stop
	}
	return stopTiming{Deadline: 120 * time.Second, Poll: 2 * time.Second}
}
func waitManager(ctx context.Context, deps runDeps, timing readinessTiming) relaunch.Readiness {
	return relaunch.WaitReady(ctx, relaunch.ReadyDeps{Status: deps.Status, Now: deps.Now, Sleep: deps.Sleep, Deadline: timing.Deadline, Poll: timing.Poll, Classify: func(status any) relaunch.Readiness { return relaunch.ClassifyStatus(status, deps.root()) }})
}
func classify(ctx context.Context, deps runDeps) (SupervisorState, error) {
	ps, err := deps.PS(ctx)
	if err != nil {
		return SupervisorState{}, err
	}
	endpoints, err := deps.Endpoints(ctx)
	if err != nil {
		return SupervisorState{}, err
	}
	eligible, err := deps.SeatNames(ctx)
	if err != nil {
		return SupervisorState{}, err
	}
	state := classifySupervisorForRoot(ps, endpoints, eligible, deps.root(), deps.Config.EndpointKind)
	return state, nil
}
func (deps runDeps) now() time.Time {
	if deps.Now == nil {
		return time.Now()
	}
	return deps.Now()
}
func (deps runDeps) sleep(ctx context.Context, d time.Duration) error {
	if deps.Sleep == nil {
		return sleepContext(ctx, d)
	}
	return deps.Sleep(ctx, d)
}
func (deps runDeps) log(ctx context.Context, line string) error {
	if deps.WriteLog != nil {
		return deps.WriteLog(line)
	}
	if deps.Log != nil {
		deps.Log(line)
		return nil
	}
	return slog.Default().Handler().Handle(ctx, slog.NewRecord(time.Now(), slog.LevelInfo, line, 0))
}
func (deps runDeps) warn(message string, err error) { slog.Warn(message, "error", err) }
func elapsedMS(deps runDeps, start time.Time) int64 { return deps.now().Sub(start).Milliseconds() }
func isoNow(deps runDeps) string                    { return deps.now().UTC().Format("2006-01-02T15:04:05.000Z") }
func failed(reason string, start time.Time, deps runDeps, state, session string) Outcome {
	return Outcome{Outcome: outcomeFailed, Reason: reason, State: state, Session: session, ElapsedMS: elapsedMS(deps, start)}
}
func classificationFailed(err error, start time.Time, deps runDeps) Outcome {
	detail, _, _ := strings.Cut(err.Error(), "\n")
	if len(detail) > 200 {
		detail = detail[:200]
	}
	out := failed("classification-failed", start, deps, "", "")
	out.Detail = detail
	return out
}
func contains(values []string, target string) bool {
	return slices.Contains(values, target)
}
func appendUnique(values []string, value string) []string {
	if contains(values, value) {
		return values
	}
	return append(values, value)
}
func sameNames(left, right []string) bool {
	return slices.Equal(left, right)
}
func pruneSpawns(values []string, now time.Time) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		stamp, err := time.Parse(time.RFC3339Nano, value)
		if err == nil && !stamp.After(now) && now.Sub(stamp) < spawnWindow {
			result = append(result, value)
		}
	}
	return result
}
func waitUntil(ctx context.Context, deps runDeps, duration time.Duration) error {
	deadline := deps.now().Add(duration)
	for deps.now().Before(deadline) {
		remaining := min(deadline.Sub(deps.now()), 2*time.Second)
		if err := deps.sleep(ctx, remaining); err != nil {
			return err
		}
	}
	return nil
}
func newestSupervisorSession(deps runDeps, burned []string) string {
	blocked := make(map[string]struct{}, len(burned))
	for _, id := range burned {
		blocked[id] = struct{}{}
	}
	pick, err := relaunch.NewestSeatSession(deps.RootCWD, deps.Paths.Home, deps.Paths.Sessions, deps.root(), blocked, relaunch.SessionDeps{FileSystem: deps.FS})
	if err != nil || pick == nil {
		return ""
	}
	return pick.ID
}
func reconcileAfter(ctx context.Context, deps runDeps, before map[string]struct{}) []relaunch.WaveAgent {
	after := relaunch.SnapshotSessionFiles(deps.Paths.Sessions, deps.FS)
	names, err := deps.SeatNames(ctx)
	if err != nil {
		deps.warn("read seats during session reconciliation", err)
		return nil
	}
	return relaunch.ReconcileSessions(before, mapKeys(after), names, deps.FS, nil)
}

func (deps runDeps) root() string {
	if deps.Root != "" {
		return deps.Root
	}
	return deps.Config.RootSeat
}

func activeRootNames(names map[string]struct{}) string {
	return waveseats.ActiveRootNames(names)
}
