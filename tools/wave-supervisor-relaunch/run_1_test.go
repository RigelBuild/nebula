package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/cotalreach"
	"github.com/RigelBuild/nebula/tools/go-lib/relaunch"
)

// testConfig is the fixture deployment config. Every value is a placeholder, so the tests carry no
// real host, unit or issue name.
func testConfig() Config {
	return Config{
		RootSeat:         "foreman",
		CotalSpace:       "main",
		EndpointKind:     "agent",
		ZellijSession:    "viewer",
		ZellijUnit:       "zellij-server.service",
		CotalManagerUnit: "cotal-manager.service",
		RelaunchUnit:     "wave-supervisor-relaunch.service",
		WaveSlice:        "wave-agents.slice",
		SeatsPath:        "/state/wave-seats.json",
		StateDir:         "/state",
		HomeDir:          "/home/fixture",
		SessionsDir:      "/sessions",
		Hosts:            []string{"seat-host-a"},
		ManagerLogPath:   "~/.cotal/manager.log",
		JournalUnits:     []string{"cotal-manager", "zellij-server", "wave-supervisor-relaunch"},
		RelaunchIssue:    "RIG-0001",
		KnownCause:       "zellij server out of memory",
		CauseOwner:       "harness",
		SeatCLI:          "wave-seats",
		SpawnCommand:     "spawn-agent",
		SeatHolderCLI:    "wave-seat-holder",
	}
}

const testLivePS = `{"name":"foreman","mesh":"idle","meshView":"current"}`

const testOrphanEndpoints = "foreman/foreman   agent     ○ idle · unchanged for 9m  thinking (set 9m ago)"

func testPSEnvelope(rows string) string {
	if rows == "" {
		rows = "[]"
	} else {
		rows = "[" + strings.TrimSpace(rows) + "]"
	}
	return `{"schemaVersion":1,"command":"ps","outcome":"ok","result":{"kind":"seats","items":` + rows + `}}`
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

type eventFS struct {
	relaunch.FileSystem
	events *[]string
}

func (f eventFS) Rename(from, to string) error {
	if err := f.FileSystem.Rename(from, to); err != nil {
		return err
	}
	if strings.Contains(to, "wave-relaunch-manifest.json") {
		*f.events = append(*f.events, "manifest-rename")
	}
	return nil
}

type failingRemoveFS struct {
	relaunch.FileSystem
	path string
	err  error
}

func (f failingRemoveFS) Remove(path string) error {
	if path == f.path {
		return f.err
	}
	return f.FileSystem.Remove(path)
}

// runHarness provides a fully fake runtime. In particular, all restart and stop
// operations record calls instead of touching services or managed processes.
type runHarness struct {
	deps       runDeps
	fs         *memoryFS
	calls      []string
	dms        []string
	events     []string
	time       time.Time
	ps         string
	endpoints  string
	identity   string
	unitProcs  string
	probe      []bool
	spawnCodes []int
	liveAfter  bool
}

func newRunHarness() *runHarness {
	h := &runHarness{
		fs:         newMemoryFS(),
		time:       time.UnixMilli(1_800_000_000_000).UTC(),
		endpoints:  "",
		identity:   "500:1",
		unitProcs:  "500\n600\n",
		probe:      []bool{true},
		spawnCodes: []int{0},
		liveAfter:  true,
	}
	initializeRunHarnessDeps(h)
	finishRunHarnessDeps(h)
	h.fs.files["/sessions/-agents-workspaces-foreman/session.jsonl"] = []byte(`{"type":"session","id":"supervisor-session","title":"foreman","cwd":"/home/mattw/agents/workspaces/foreman"}`)
	h.fs.times["/sessions/-agents-workspaces-foreman/session.jsonl"] = h.time
	return h
}

func initializeRunHarnessDeps(h *runHarness) {
	h.deps = runDeps{
		Config: testConfig(),
		FS:     eventFS{FileSystem: h.fs, events: &h.events},
		Paths: toolPaths{
			State:          "/state/wave-supervisor-relaunch.json",
			Hold:           "/state/wave-relaunch-hold.json",
			Manifest:       "/state/wave-relaunch-manifest.json",
			RestartRequest: "/state/wave-zellij-restart-request.json",
			Sessions:       "/sessions",
			Home:           "/home/mattw",
			Proc:           "/proc",
			Seats:          "/state/wave-seats.json",
		},
		Now: func() time.Time { return h.time },
		Sleep: func(ctx context.Context, delay time.Duration) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			h.time = h.time.Add(delay)
			return nil
		},
		Status: func(context.Context) (any, error) {
			h.calls = append(h.calls, "status")
			return map[string]any{"state": "idle", "lastSweep": map[string]any{"completedAt": "done"}, "failures": []any{}}, nil
		},
		PS: func(context.Context) (string, error) {
			h.calls = append(h.calls, "ps")
			return testPSEnvelope(h.ps), nil
		},
		Endpoints: func(context.Context) (string, error) {
			h.calls = append(h.calls, "endpoints")
			return h.endpoints, nil
		},
		Trigger: func(context.Context) (Trigger, error) {
			return Trigger{BootID: "boot", ManagerUnit: "cotal-manager.service", Units: []UnitFact{{Unit: "cotal-manager.service", InvocationID: "manager", ActiveState: "active"}}, ZellijServer: h.identity}, nil
		},
		StopSeats: func(_ context.Context, names []string) error {
			for _, name := range names {
				call := "cotal stop --name " + name + " --space main"
				h.calls = append(h.calls, call)
				h.events = append(h.events, call)
			}
			return nil
		},
		StopSlice: func(context.Context) (bool, error) {
			h.calls = append(h.calls, "stop-slice")
			h.events = append(h.events, "stop-slice")
			return true, nil
		},
		ProcScopesPresent: func() bool { return false },
		SeatNames: func(context.Context) (map[string]struct{}, error) {
			return map[string]struct{}{"foreman": {}, "accounts": {}, "runners": {}}, nil
		},
		ReadRootCWD: func(context.Context) (string, error) { return "/home/mattw/agents/workspaces/foreman", nil },
	}
}

func runCommandForTest(ctx context.Context, args []string, deps runDeps) int {
	return runCommandWithWritersForTest(ctx, args, deps, io.Discard)
}

func runCommandWithWritersForTest(ctx context.Context, args []string, deps runDeps, stderr io.Writer) int {
	root := newRootCommand(ctx, deps, io.Discard, stderr)
	root.SetArgs(args)
	root.SetIn(strings.NewReader(""))
	if err := root.ExecuteContext(ctx); err != nil {
		return commandStatus(err)
	}
	return 0
}

func (h *runHarness) write(path, text string) { h.fs.files[path] = []byte(text) }

func (h *runHarness) hasCall(call string) bool {
	return slices.Contains(h.calls, call)
}

func (h *runHarness) countPrefix(prefix string) int {
	count := 0
	for _, call := range h.calls {
		if strings.HasPrefix(call, prefix) {
			count++
		}
	}
	return count
}

// requireSingleSpawnPrefix fails unless exactly one spawn call starts with prefix.
func (h *runHarness) requireSingleSpawnPrefix(t *testing.T, prefix string) {
	t.Helper()
	spawnCalls := make([]string, 0, 1)
	for _, call := range h.calls {
		if strings.HasPrefix(call, "spawn:") {
			spawnCalls = append(spawnCalls, call)
		}
	}
	if len(spawnCalls) != 1 || !strings.HasPrefix(spawnCalls[0], prefix) {
		t.Fatalf("spawn calls = %v, want one call starting %q", spawnCalls, prefix)
	}
}

func (h *runHarness) installWaveScope() {
	h.fs.files["/proc/111/cgroup"] = []byte("0::/wave-agents.slice/scope")
	h.fs.files["/proc/111/cmdline"] = []byte("omp\x00--fork\x00parent-session\x00")
	h.fs.files["/proc/111/environ"] = []byte("COTAL_AGENT_FILE=/agents/foreman.md\x00HOME=/home/mattw\x00")
	h.fs.links["/proc/111/fd/1"] = "/sessions/-home-mattw-agents/session.jsonl"
	h.fs.files["/sessions/-home-mattw-agents/session.jsonl"] = []byte(`{"type":"session","id":"census-supervisor","parentSession":"parent-session"}`)
	h.fs.times["/sessions/-home-mattw-agents/session.jsonl"] = h.time
}

func TestCotalFailureFormatsCommandAndFirstStderrLine(t *testing.T) {
	if got := cotalFailure([]string{"ps", "--space"}, cotalreach.CommandResult{ExitCode: 3, Stderr: "\n  nats: timeout\nmore"}, 31878*time.Millisecond); got != "cotal ps exit=3 ms=31878 stderr=nats: timeout" {
		t.Fatalf("cotalFailure() = %q", got)
	}
	if got := cotalFailure([]string{"endpoints"}, cotalreach.CommandResult{ExitCode: 1}, 5*time.Millisecond); got != "cotal endpoints exit=1 ms=5" {
		t.Fatalf("cotalFailure() = %q", got)
	}

}

func TestClassificationFailureDetail(t *testing.T) {
	h := newRunHarness()
	var lines []string
	h.deps.WriteLog = func(line string) error { lines = append(lines, line); return nil }
	h.deps.PS = func(context.Context) (string, error) {
		return "", errors.New("cotal ps exit=3 ms=87574 stderr=nats: timeout")
	}
	got := runSupervisor(t.Context(), h.deps)
	root := newRootCommand(t.Context(), h.deps, io.Discard, io.Discard)
	root.SetArgs([]string{"run"})
	if err := root.ExecuteContext(t.Context()); commandStatus(err) != 1 {
		t.Fatalf("run status=%d error=%v", commandStatus(err), err)
	}
	if got.Reason != "classification-failed" || got.Detail != "cotal ps exit=3 ms=87574 stderr=nats: timeout" {
		t.Fatalf("outcome=%#v", got)
	}
	want := `detail="cotal ps exit=3 ms=87574 stderr=nats: timeout"`
	if len(lines) != 1 || !strings.Contains(lines[0], want) {
		t.Fatalf("summary lines=%q", lines)
	}
}

func TestRunSelectsActiveForemanNotSupervisor(t *testing.T) {
	h := newRunHarness()
	h.deps.SeatNames = func(context.Context) (map[string]struct{}, error) {
		return map[string]struct{}{"supervisor": {}, "foreman": {}}, nil
	}
	h.ps = testLivePS
	h.endpoints = "supervisor/supervisor agent ○ idle\nforeman/foreman agent ○ idle"
	h.write(h.deps.Paths.State, `{"version":1,"spawns":[],"burned":[],"lastFingerprint":"old-boot:manager:devbox=none"}`)
	got := runSupervisor(t.Context(), h.deps)
	if got.Reason != "trigger-changed" {
		t.Fatalf("run reason = %q, want trigger-changed (%#v)", got.Reason, got)
	}
	if !slices.Contains(h.dms, "foreman") {
		t.Fatalf("DM must go to foreman, dms = %v", h.dms)
	}
	if slices.Contains(h.dms, "supervisor") {
		t.Fatalf("DM must not go to supervisor, dms = %v", h.dms)
	}
}

func TestRunSkipsWithoutActiveRoot(t *testing.T) {
	h := newRunHarness()
	h.deps.SeatNames = func(context.Context) (map[string]struct{}, error) {
		return map[string]struct{}{"worker": {}}, nil
	}
	if got := runSupervisor(t.Context(), h.deps); got.Reason != "root-seat-inactive" {
		t.Fatalf("run = %#v", got)
	}
}

func TestRunHoldAndReadiness(t *testing.T) {
	t.Run("active hold skips before any manager calls", func(t *testing.T) {
		h := newRunHarness()
		h.write(h.deps.Paths.Hold, fmt.Sprintf(`{"until":%q,"reason":"test"}`, h.time.Add(time.Minute).Format(time.RFC3339Nano)))
		if got := runSupervisor(t.Context(), h.deps); got.Reason != "hold" {
			t.Fatalf("run = %#v", got)
		}
		if len(h.calls) != 0 {
			t.Fatalf("calls = %v", h.calls)
		}
	})
	t.Run("active hold skips when the seat list is unreadable", func(t *testing.T) {
		h := newRunHarness()
		h.write(h.deps.Paths.Hold, fmt.Sprintf(`{"until":%q,"reason":"test"}`, h.time.Add(time.Minute).Format(time.RFC3339Nano)))
		h.deps.SeatNames = func(context.Context) (map[string]struct{}, error) {
			return nil, errors.New("seat list denied")
		}
		if got := runSupervisor(t.Context(), h.deps); got.Outcome != outcomeSkipped || got.Reason != "hold" {
			t.Fatalf("run = %#v", got)
		}
	})
	t.Run("malformed state fails closed", func(t *testing.T) {
		h := newRunHarness()
		h.write(h.deps.Paths.State, "{")
		if got := runSupervisor(t.Context(), h.deps); got.Reason != "state-unreadable" {
			t.Fatalf("run = %#v", got)
		}
		if len(h.calls) != 0 {
			t.Fatalf("calls = %v", h.calls)
		}
	})
	t.Run("readiness deadline reports manager not ready", func(t *testing.T) {
		h := newRunHarness()
		h.deps.Status = func(context.Context) (any, error) {
			return nil, nil //nolint:nilnil // Exercise an unavailable status.
		}
		if got := runSupervisor(t.Context(), h.deps); got.Reason != "manager-not-ready" {
			t.Fatalf("run = %#v", got)
		}
	})
	t.Run("manager stopping skips before classification", func(t *testing.T) {
		h := newRunHarness()
		h.deps.Trigger = func(context.Context) (Trigger, error) {
			return Trigger{BootID: "boot", ManagerUnit: "cotal-manager.service", Units: []UnitFact{{Unit: "cotal-manager.service", InvocationID: "manager", ActiveState: "deactivating"}}}, nil
		}
		if got := runSupervisor(t.Context(), h.deps); got.Reason != "manager-stopping" {
			t.Fatalf("run=%#v", got)
		}
		if h.hasCall("ps") || h.hasCall("endpoints") || h.countPrefix("spawn:") != 0 {
			t.Fatalf("manager-stop run classified or spawned: %v", h.calls)
		}
	})
}

func TestRunLiveSupervisorNotices(t *testing.T) {
	t.Run("same fingerprint skips unchanged", testRunLiveSupervisorNoticesCase1)
	for _, tc := range []struct {
		name, fingerprint, server, want string
		wantDM                          bool
	}{
		{name: "old zellij identity is silently rebaselined", fingerprint: "boot:manager:old-zellij-unit-id", server: "500:1", want: "rebaselined"},
		{name: "manager change notifies", fingerprint: "boot:old-manager:zellij", server: "500:1", want: "trigger-changed", wantDM: true},
		{name: "boot change notifies", fingerprint: "old-boot:manager:zellij", server: "500:1", want: "trigger-changed", wantDM: true},
		{name: "unreadable unchanged server waits", fingerprint: "boot:manager:devbox=500:1", server: "unavailable", want: "devbox-unavailable"},
		{name: "unreadable old baseline waits", fingerprint: "boot:manager:devbox=unavailable", server: "unavailable", want: "devbox-unavailable"},
		{name: "unavailable baseline is not replaced by no server", fingerprint: "boot:manager:devbox=unavailable", server: "none", want: "trigger-changed", wantDM: true},
		{name: "legacy baseline is not replaced by no server", fingerprint: "boot:manager:old-zellij-unit-id", server: "none", want: "trigger-changed", wantDM: true},
		{name: "unreadable after manager change notifies", fingerprint: "boot:old-manager:devbox=500:1", server: "unavailable", want: "trigger-changed", wantDM: true},
		{name: "unreadable after boot change notifies", fingerprint: "old-boot:manager:devbox=500:1", server: "unavailable", want: "trigger-changed", wantDM: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testRunLiveSupervisorNoticesCase2(t, tc.fingerprint, tc.server, tc.want, tc.wantDM)
		})
	}
	t.Run("first arm and changed fingerprint send one DM", testRunLiveSupervisorNoticesCase3)
	t.Run("unreadable server arms once and adopts a later readable identity", testRunLiveSupervisorNoticesUnreadableServerArmsOnceAndAdopts)
	t.Run("notice inside fifteen minutes is rate limited", testRunLiveSupervisorNoticesCase4)
	t.Run("starting supervisor skips", testRunLiveSupervisorNoticesCase5)
}

func TestRootCommandErrors(t *testing.T) {
	t.Run("flag parsing writes usage and returns status two", func(t *testing.T) {
		root := newRootCommand(t.Context(), newRunHarness().deps, io.Discard, io.Discard)
		root.SetArgs([]string{"--bad"})
		if err := root.ExecuteContext(context.Background()); commandStatus(err) != 2 {
			t.Fatalf("error=%v status=%d", err, commandStatus(err))
		}
	})
	t.Run("error status stays nonzero when stderr write fails", func(t *testing.T) {
		if got := writeErrorStatus(errWriter{}, errors.New("runtime error"), 1); got != 1 {
			t.Fatalf("status=%d", got)
		}
	})
}

func testRunLiveSupervisorNoticesUnreadableServerArmsOnceAndAdopts(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.ps = testLivePS
	h.identity = "unavailable"
	var notices []string
	h.deps.DM = func(_ context.Context, _ string, text string) (bool, error) {
		notices = append(notices, text)
		return true, nil
	}
	if got := runSupervisor(t.Context(), h.deps); got.Outcome != "notified" || got.Reason != "armed" {
		t.Fatalf("first run=%#v", got)
	}
	state, valid := relaunch.ReadState(h.deps.Paths.State, h.fs)
	if !valid || state.LastFingerprint != "boot:manager:devbox=unavailable" || len(notices) != 1 || !strings.HasPrefix(notices[0], "Wave relaunch armed with no prior baseline") {
		t.Fatalf("armed state=%#v notices=%q valid=%t", state, notices, valid)
	}
	h.time = h.time.Add(time.Hour)
	if got := runSupervisor(t.Context(), h.deps); got.Reason != "devbox-unavailable" {
		t.Fatalf("unreadable repeat=%#v", got)
	}
	h.identity = "500:1"
	if got := runSupervisor(t.Context(), h.deps); got.Reason != "rebaselined" {
		t.Fatalf("readable recovery=%#v", got)
	}
	state, valid = relaunch.ReadState(h.deps.Paths.State, h.fs)
	if !valid || state.LastFingerprint != "boot:manager:devbox=500:1" || len(notices) != 1 {
		t.Fatalf("recovered state=%#v notices=%q valid=%t", state, notices, valid)
	}
	if got := runSupervisor(t.Context(), h.deps); got.Reason != "unchanged" {
		t.Fatalf("stable readable repeat=%#v", got)
	}
}
func finishRunHarnessDeps(h *runHarness) {
	h.deps.EndpointsClear = func(context.Context) (bool, error) { return true, nil }
	h.deps.ZellijSessions = func(context.Context) (string, error) { return "devbox [Created 1h ago]", nil }
	h.deps.ProbeZellij = func(context.Context) (bool, error) {
		h.calls = append(h.calls, "probe")
		if len(h.probe) == 0 {
			return true, nil
		}
		value := h.probe[0]
		h.probe = h.probe[1:]
		return value, nil
	}
	h.deps.ZellijIdentity = func(context.Context) string { return h.identity }
	h.deps.ZellijUnitProcs = func(context.Context) (string, error) { return h.unitProcs, nil }
	h.deps.RestartZellij = func(context.Context) (bool, error) {
		h.calls = append(h.calls, "restart")
		h.identity = "600:2"
		return true, nil
	}
	h.deps.StartRelaunch = func(context.Context) (bool, error) {
		h.calls = append(h.calls, "start-relaunch")
		return true, nil
	}
	h.deps.Spawn = func(_ context.Context, args []string, _ string) (int, error) {
		h.calls = append(h.calls, "spawn:"+strings.Join(args, " "))
		h.events = append(h.events, "spawn")
		code := 0
		if len(h.spawnCodes) != 0 {
			code, h.spawnCodes = h.spawnCodes[0], h.spawnCodes[1:]
		}
		if (code == 0 || code == cotalreach.VIEW_FAILED) && h.liveAfter {
			h.ps = testLivePS
		}
		return code, nil
	}
	h.deps.DM = func(_ context.Context, recipient, _ string) (bool, error) {
		h.calls = append(h.calls, "dm")
		h.dms = append(h.dms, recipient)
		return true, nil
	}
	h.deps.Log = func(string) {}
	h.deps.Readiness = &readinessTiming{Deadline: time.Second, Poll: time.Millisecond}
	h.deps.Stop = &stopTiming{Deadline: time.Second, Poll: time.Millisecond}
	h.deps.Post = &readinessTiming{Deadline: time.Second, Poll: time.Millisecond}
}

func testRunLiveSupervisorNoticesCase1(t *testing.T) {
	h := newRunHarness()
	h.ps = testLivePS
	h.write(h.deps.Paths.State, `{"version":1,"spawns":[],"burned":[],"lastFingerprint":"boot:manager:devbox=500:1"}`)
	if got := runSupervisor(t.Context(), h.deps); got.Reason != "unchanged" || got.State != "live" {
		t.Fatalf("run = %#v", got)
	}
	if h.hasCall("dm") {
		t.Fatalf("unexpected DM: %v", h.calls)
	}
}

func testRunLiveSupervisorNoticesCase2(t *testing.T, fingerprint, server, want string, wantDM bool) {
	t.Helper()
	h := newRunHarness()
	h.ps = testLivePS
	h.identity = server
	h.write(h.deps.Paths.State, fmt.Sprintf(`{"version":1,"spawns":[],"burned":[],"lastFingerprint":%q}`, fingerprint))
	got := runSupervisor(t.Context(), h.deps)
	if got.Reason != want {
		t.Fatalf("run reason = %q, want %q (%#v)", got.Reason, want, got)
	}
	if h.hasCall("dm") != wantDM {
		t.Fatalf("DM calls = %v, want %t", h.calls, wantDM)
	}
}

func testRunLiveSupervisorNoticesCase3(t *testing.T) {
	for _, seeded := range []bool{false, true} {
		h := newRunHarness()
		h.ps = testLivePS
		if seeded {
			h.write(h.deps.Paths.State, `{"version":1,"spawns":[],"burned":[],"lastFingerprint":"boot:other:devbox=none"}`)
		}
		got := runSupervisor(t.Context(), h.deps)
		if got.Outcome != "notified" || h.countPrefix("dm") != 1 {
			t.Fatalf("run=%#v calls=%v", got, h.calls)
		}
	}
}

func testRunLiveSupervisorNoticesCase4(t *testing.T) {
	h := newRunHarness()
	h.ps = testLivePS
	h.write(h.deps.Paths.State, fmt.Sprintf(`{"version":1,"spawns":[],"burned":[],"lastFingerprint":"boot:other:devbox=none","lastNoticeAt":%q}`, h.time.Add(-time.Minute).Format(time.RFC3339Nano)))
	if got := runSupervisor(t.Context(), h.deps); got.Reason != "notice-rate" || h.hasCall("dm") {
		t.Fatalf("run=%#v calls=%v", got, h.calls)
	}
}
func testRunLiveSupervisorNoticesCase5(t *testing.T) {
	h := newRunHarness()
	h.ps = `{"name":"foreman","mesh":"starting"}`
	if got := runSupervisor(t.Context(), h.deps); got.Reason != "starting" {
		t.Fatalf("run = %#v", got)
	}
}
