package main

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/relaunch"
	"github.com/RigelBuild/nebula/tools/go-lib/waveseats"
)

func TestCoreReadinessAndClassification(t *testing.T) {
	t.Run("classifies failed calls, incomplete sweeps, retry states and ready retry-wait", func(t *testing.T) {
		cases := []struct {
			name   string
			status any
			kind   relaunch.ReadinessKind
		}{
			{name: "status unavailable", kind: relaunch.ReadinessWaiting},
			{name: "running", status: map[string]any{"state": "running", "lastSweep": map[string]any{"completedAt": "now"}}, kind: relaunch.ReadinessWaiting},
			{name: "sweep error", status: map[string]any{"state": "idle", "lastSweep": map[string]any{"error": "failed", "completedAt": "now"}}, kind: relaunch.ReadinessBlocked},
			{name: "failed", status: map[string]any{"state": "failed", "lastSweep": map[string]any{"completedAt": "now"}}, kind: relaunch.ReadinessBlocked},
			{name: "retry-wait", status: map[string]any{"state": "retry-wait", "lastSweep": map[string]any{"completedAt": "now"}, "failures": []any{}}, kind: relaunch.ReadinessReady},
			{name: "retrying", status: map[string]any{"state": "retrying", "lastSweep": map[string]any{"completedAt": "now"}, "failures": []any{}}, kind: relaunch.ReadinessWaiting},
			{name: "retry-scheduled", status: map[string]any{"state": "idle", "lastSweep": map[string]any{"completedAt": "now"}, "failures": []any{map[string]any{"alias": "foreman", "phase": "retry-scheduled"}}}, kind: relaunch.ReadinessWaiting},
			{name: "refused", status: map[string]any{"state": "idle", "lastSweep": map[string]any{"completedAt": "now"}, "failures": []any{map[string]any{"alias": "foreman", "disposition": "refused"}}}, kind: relaunch.ReadinessBlocked},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := relaunch.ClassifyStatus(tc.status, "foreman"); got.Kind != tc.kind {
					t.Fatalf("ClassifyStatus() = %#v, want kind %q", got, tc.kind)
				}
			})
		}
	})

	t.Run("waits by condition poll with injected clock and sleep", func(t *testing.T) {
		now := time.Unix(0, 0)
		attempts := 0
		got := relaunch.WaitReady(t.Context(), relaunch.ReadyDeps{
			Status: func(_ context.Context) (any, error) {
				attempts++
				if attempts < 3 {
					// A missing status is the condition this fixture exercises.
					return nil, nil //nolint:nilnil // A missing status is valid here.
				}
				return map[string]any{"state": "idle", "lastSweep": map[string]any{"completedAt": "done"}}, nil
			},
			Now: func() time.Time { return now },
			Sleep: func(_ context.Context, delay time.Duration) error {
				now = now.Add(delay)
				return nil
			},
			Deadline: 10 * time.Millisecond,
			Poll:     2 * time.Millisecond,
		})
		if got.Kind != relaunch.ReadinessReady || attempts != 3 {
			t.Fatalf("WaitReady() = %#v after %d attempts", got, attempts)
		}
	})

	t.Run("managerReady ignores per-alias failures that block the supervisor", func(t *testing.T) {
		status := map[string]any{"state": "idle", "lastSweep": map[string]any{"completedAt": "now"}, "failures": []any{map[string]any{"alias": "foreman", "disposition": "refused"}}}
		if got := relaunch.ManagerReady(status); got.Kind != relaunch.ReadinessReady {
			t.Fatalf("ManagerReady() = %#v", got)
		}
		if got := relaunch.ManagerReady(map[string]any{"state": "running", "lastSweep": map[string]any{"completedAt": "now"}}); got.Kind != relaunch.ReadinessWaiting {
			t.Fatalf("ManagerReady(running) = %#v", got)
		}
	})
}

func TestCoreTriggerAndSupervisorState(t *testing.T) {
	t.Run("parses unit show records; fingerprint keys the manager invocation and the devbox server", func(t *testing.T) {
		facts := parseUnitFacts("Id=cotal-manager.service\nActiveEnterTimestamp=Sun 2026-11-01 02:52:00 UTC\nInvocationID=manager-id\nNRestarts=2\nActiveState=active\nResult=success\n\nId=zellij-server.service\nActiveEnterTimestamp=Sun 2026-11-01 02:50:00 UTC\nInvocationID=zellij-id\nNRestarts=1\nActiveState=active\nResult=success\n")
		trigger := Trigger{BootID: "boot-fixture", ManagerUnit: "cotal-manager.service", Units: facts, ZellijServer: "3845913:9828715"}
		if len(facts) != 2 || facts[0].InvocationID != "manager-id" || fingerprint(trigger) != "boot-fixture:manager-id:devbox=3845913:9828715" {
			t.Fatalf("facts=%#v fingerprint=%q", facts, fingerprint(trigger))
		}
	})

	t.Run("a failed zellij-server re-run with devbox still up keeps the fingerprint", func(t *testing.T) {
		trigger := Trigger{BootID: "boot", ManagerUnit: "cotal-manager.service", Units: []UnitFact{{Unit: "cotal-manager.service", InvocationID: "manager"}, {Unit: "zellij-server.service", InvocationID: "zellij", ActiveState: "active", Result: "success"}}, ZellijServer: "500:1"}
		rerun := trigger
		rerun.Units = append([]UnitFact(nil), trigger.Units...)
		rerun.Units[1].InvocationID, rerun.Units[1].ActiveState, rerun.Units[1].Result = "new", "failed", "exit-code"
		if fingerprint(rerun) != fingerprint(trigger) || fingerprint(Trigger{BootID: "boot", ManagerUnit: "cotal-manager.service", Units: trigger.Units, ZellijServer: "400:2"}) == fingerprint(trigger) || fingerprint(Trigger{BootID: "boot", ManagerUnit: "cotal-manager.service", Units: trigger.Units, ZellijServer: "none"}) == fingerprint(trigger) {
			t.Fatal("fingerprint included zellij unit fields or ignored devbox identity")
		}
	})

	t.Run("zellijServerIdentity reads only the systemctl MainPID and keys it on pid and start time", testCoreTriggerAndSupervisorStateDevboxServerIdentityFindsTheDevboxServerBy)

	t.Run("zellijServerIdentity reports an unreadable pid as unavailable and an exited pid as gone", testCoreTriggerAndSupervisorStateDevboxServerIdentityReportsAnIncompleteScanAs)

	t.Run("zellijServerIdentity matches only this user's runtime dir and contract version, and fails closed on an unreadable pid", testCoreTriggerAndSupervisorStateDevboxServerIdentityMatchesOnlyThisUserS)

	t.Run("zellijServerIdentity finds the MainPID server on a hashed socket name", testCoreTriggerAndSupervisorStateDevboxServerIdentityFindsTheMainPIDServerOnAHashedSocket)

	t.Run("zellijServerIdentity ignores a MainPID whose cmdline is not a zellij server", testCoreTriggerAndSupervisorStateDevboxServerIdentityIgnoresANonZellijMainPID)

	t.Run("zellijServerIdentity is none when MainPID is zero", testCoreTriggerAndSupervisorStateDevboxServerIdentityIsNoneWhenMainPIDIsZero)

	t.Run("zellijServerIdentity ignores a non-MainPID test server", testCoreTriggerAndSupervisorStateDevboxServerIdentityIgnoresANonMainPIDTestServer)

	t.Run("zellijServerIdentity fails closed when the systemctl MainPID read fails or is unparseable", testCoreTriggerAndSupervisorStateDevboxServerIdentityFailsClosedWhenSystemctlFails)

	lanes := map[string]struct{}{"accounts": {}, "runners": {}}
	t.Run("recognizes the captured orphan endpoint while ignoring manager and nemo", func(t *testing.T) {
		state := classifySupervisor(testPSEnvelope(""), coreEndpoints, "foreman", "agent", lanes)
		if state.Kind != "orphaned" || !sameNames(state.OrphanSet, []string{"foreman"}) {
			t.Fatalf("classifySupervisor() = %#v", state)
		}
	})
	t.Run("treats an unparsable ps row as starting and any lane ps row as present", func(t *testing.T) {
		if got := classifySupervisor(testPSEnvelope("{\"name\":\"foreman\",\"mesh\":\"starting\"}\n"), coreEndpoints, "foreman", "agent", lanes); got.Kind != "starting" {
			t.Fatalf("starting = %#v", got)
		}
		if got := classifySupervisor(testPSEnvelope("{\"name\":\"accounts\",\"mesh\":\"starting\"}\n"), "accounts/worker agent ○ idle", "foreman", "agent", lanes); got.Kind != "absent" {
			t.Fatalf("lane-only state = %#v", got)
		}
	})
	t.Run("ignores manager, nemo, offline and ineligible endpoint rows", func(t *testing.T) {
		rows := "manager/manager endpoint ○ idle\nnemo/nemo agent ○ idle\nunknown/name agent ○ idle\naccounts/worker agent ● offline"
		if got := classifySupervisor(testPSEnvelope(""), rows, "foreman", "agent", lanes); got.Kind != "absent" {
			t.Fatalf("classifySupervisor() = %#v", got)
		}
	})
	t.Run("parses bracketed-id and role-less endpoint rows", func(t *testing.T) {
		got := classifySupervisor(testPSEnvelope(""), "foreman [ab12] agent ○ idle\naccounts agent ○ idle", "foreman", "agent", lanes)
		if got.Kind != "orphaned" || !sameNames(got.OrphanSet, []string{"accounts", "foreman"}) {
			t.Fatalf("classifySupervisor() = %#v", got)
		}
	})
	t.Run("orphan workers under a live supervisor never stop the wave; malformed ps is invalid", testCoreTriggerAndSupervisorStateOrphanWorkersUnderALiveSupervisor)
}

const coreEndpoints = "foreman/foreman   agent     ○ idle · unchanged for 9m  thinking (set 9m ago)\nmanager/manager         endpoint  ○ idle · unchanged for 40m  agent host (zellij) (set 40m ago)\nnemo/nemo               agent     ● working · progress unknown · unchanged for 8s  thinking (set 8s ago)"

func TestCoreSessionHelpersAndPresentation(t *testing.T) {
	t.Run("sessionDirFor slugs home-relative and absolute cwds", testCoreSessionHelpersAndPresentationSessionDirForSlugsHomeRelativeAndAbsolute)

	t.Run("resolves open, parent, session-id, and stale sessions", testCoreSessionHelpersAndPresentationResolvesOpenParentSessionIdAnd)

	t.Run("reconciles newly-created sessions for eligible titles only", testCoreSessionHelpersAndPresentationCase1)

	t.Run("picks by mtime, not filename, and skips burned session IDs", testCoreSessionHelpersAndPresentationCase2)

	t.Run("relaunchPrompt never asks the supervisor to spawn a lane", testCoreSessionHelpersAndPresentationCase4)

	t.Run("live notices describe a down or unreadable devbox server in words", testCoreSessionHelpersAndPresentationCase5)

	t.Run("formats failure summaries and suppresses normal stop-post results", testCoreSessionHelpersAndPresentationCase6)

	t.Run("a SIGTERM stop is a stop, while a SIGKILL or core dump still reports unit-result", testCoreSessionHelpersAndPresentationCase7)
}

func TestReadActiveSeatNamesUsesV2StatesAndCallerContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wave-seats.json")
	newSeat := func(name string, state waveseats.SeatStateV2) waveseats.SeatEntryV2 {
		return waveseats.SeatEntryV2{
			Name: name, SeatRevision: 1, CWD: "/work/" + name, Tab: name,
			State: state, UpdatedAt: "2026-11-04T12:34:56.000Z",
			Profile: "worker", Role: "worker", Workspace: "/work/" + name, Persona: name,
			Key: waveseats.KeySettings{Routing: map[waveseats.Provider]any{}, PolicyID: "policy"},
		}
	}
	list := waveseats.SeatListV2{
		Version:  2,
		Revision: 3,
		Seats: []waveseats.SeatEntryV2{
			newSeat("active-lane", waveseats.SeatV2Active),
			newSeat("paused-lane", waveseats.SeatV2Paused),
			newSeat("retired-lane", waveseats.SeatV2Retired),
		},
	}
	data, err := waveseats.SerializeSeatListV2(list, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	names, err := readActiveSeatNames(t.Context(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"active-lane"} {
		if _, ok := names[name]; !ok {
			t.Errorf("active names missing %q: %v", name, names)
		}
	}
	for _, name := range []string{"foreman", "paused-lane", "retired-lane"} {
		if _, ok := names[name]; ok {
			t.Errorf("inactive seat %q is eligible: %v", name, names)
		}
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := readActiveSeatNames(canceled, path, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("readActiveSeatNames(cancelled) error = %v, want context.Canceled", err)
	}
	if _, err := readActiveSeatNames(t.Context(), filepath.Join(t.TempDir(), "missing.json"), nil); err == nil {
		t.Fatal("readActiveSeatNames() accepted a missing seat list")
	}
	testSeatPathsFromConfig(t)

}

type memoryFS struct {
	files       map[string][]byte
	links       map[string]string
	times       map[string]time.Time
	readDirErr  error
	readHook    func(string) ([]byte, error, bool)
	readBytes   map[string]int
	readDirHook func(string) ([]fs.DirEntry, error, bool)
}

func newMemoryFS() *memoryFS {
	return &memoryFS{files: make(map[string][]byte), links: make(map[string]string), times: make(map[string]time.Time), readBytes: make(map[string]int)}
}

func (f *memoryFS) ReadFile(path string) ([]byte, error) {
	if f.readHook != nil {
		if data, err, handled := f.readHook(path); handled {
			return data, err
		}
	}
	data, ok := f.files[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	f.readBytes[path] = len(data)
	return data, nil
}

func (f *memoryFS) ReadDir(path string) ([]fs.DirEntry, error) {
	if f.readDirHook != nil {
		if entries, err, handled := f.readDirHook(path); handled {
			return entries, err
		}
	}
	if f.readDirErr != nil {
		return nil, f.readDirErr
	}
	if _, ok := f.files[path]; ok {
		return nil, &fs.PathError{Op: "readdirent", Path: path, Err: syscall.ENOTDIR}
	}
	prefix := filepath.Clean(path) + string(filepath.Separator)
	dirs := make(map[string]bool)
	for _, file := range append(slices.Collect(maps.Keys(f.files)), slices.Collect(maps.Keys(f.links))...) {
		if rest, ok := strings.CutPrefix(file, prefix); ok {
			name, _, nested := strings.Cut(rest, string(filepath.Separator))
			dirs[name] = dirs[name] || nested
		}
	}
	entries := make([]fs.DirEntry, 0, len(dirs))
	for name, dir := range dirs {
		entries = append(entries, memoryDirEntry{name: name, dir: dir})
	}
	return entries, nil
}

// ReadPrefix returns a bounded prefix without recording a whole-file read.
func (f *memoryFS) ReadPrefix(path string, maxBytes int) ([]byte, error) {
	if f.readHook != nil {
		if data, err, handled := f.readHook(path); handled {
			return data, err
		}
	}
	data, ok := f.files[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return data[:min(len(data), maxBytes)], nil
}

// ReadTail serves a bounded tail read, so a caller that reads only the last record never hits ReadFile.
func (f *memoryFS) ReadTail(path string, maxBytes int) ([]byte, error) {
	if f.readHook != nil {
		if data, err, handled := f.readHook(path); handled {
			return data, err
		}
	}
	data, ok := f.files[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return data[max(0, len(data)-maxBytes):], nil
}

func (f *memoryFS) Stat(path string) (fs.FileInfo, error) {
	if _, ok := f.files[path]; ok {
		return memoryFileInfo{name: filepath.Base(path), mod: f.times[path]}, nil
	}
	prefix := filepath.Clean(path) + string(filepath.Separator)
	for file := range f.files {
		if strings.HasPrefix(file, prefix) {
			return memoryFileInfo{name: filepath.Base(path), dir: true}, nil
		}
	}
	return nil, fs.ErrNotExist
}

func (f *memoryFS) Readlink(path string) (string, error) {
	link, ok := f.links[path]
	if !ok {
		return "", fs.ErrNotExist
	}
	return link, nil
}

func (f *memoryFS) WriteFile(path string, data []byte, _ fs.FileMode) error {
	f.files[path] = append([]byte(nil), data...)
	return nil
}

func (*memoryFS) MkdirAll(string, fs.FileMode) error { return nil }

func (f *memoryFS) Rename(from, to string) error {
	data, ok := f.files[from]
	if !ok {
		return fs.ErrNotExist
	}
	f.files[to] = data
	delete(f.files, from)
	return nil
}

func (f *memoryFS) Remove(path string) error {
	if _, ok := f.files[path]; !ok {
		return fs.ErrNotExist
	}
	delete(f.files, path)
	return nil
}

type memoryDirEntry struct {
	name string
	dir  bool
}

func (e memoryDirEntry) Name() string { return e.name }

func (e memoryDirEntry) IsDir() bool { return e.dir }

func (e memoryDirEntry) Type() fs.FileMode {
	if e.dir {
		return fs.ModeDir
	}
	return 0
}

func (e memoryDirEntry) Info() (fs.FileInfo, error) {
	return memoryFileInfo{name: e.name, dir: e.dir}, nil
}

type memoryFileInfo struct {
	name string
	dir  bool
	mod  time.Time
}

func (i memoryFileInfo) Name() string { return i.name }
func (i memoryFileInfo) Size() int64  { return 0 }
func (i memoryFileInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}
func (i memoryFileInfo) ModTime() time.Time { return i.mod }
func (i memoryFileInfo) IsDir() bool        { return i.dir }
func (i memoryFileInfo) Sys() any           { return nil }
func procStat(start string) string {
	fields := make([]string, 20)
	for index := range fields {
		fields[index] = "1"
	}
	fields[19] = start
	return "7 (zellij) " + strings.Join(fields, " ")
}
func testCoreSessionHelpersAndPresentationCase1(t *testing.T) {
	fsys := newMemoryFS()
	fsys.files["/sessions/lane.jsonl"] = []byte(`{"type":"session","id":"lane-id","title":"accounts"}`)
	fsys.files["/sessions/nemo.jsonl"] = []byte(`{"type":"session","id":"not-used","title":"nemo"}`)
	rows := relaunch.ReconcileSessions(map[string]struct{}{"old.jsonl": {}}, []string{"old.jsonl", "/sessions/lane.jsonl", "/sessions/nemo.jsonl"}, map[string]struct{}{"accounts": {}}, fsys, nil)
	if len(rows) != 1 || rows[0].Name != "accounts" || rows[0].SessionID != "lane-id" {
		t.Fatalf("ReconcileSessions() = %#v", rows)
	}
}

func testCoreSessionHelpersAndPresentationCase2(t *testing.T) {
	fsys := newMemoryFS()
	fsys.files["/sessions/new-name.jsonl"] = []byte(`{"type":"session","id":"older","title":"foreman","cwd":"/home/mattw/agents/workspaces/foreman"}`)
	fsys.files["/sessions/old-name.jsonl"] = []byte(`{"type":"session","id":"newer","title":"foreman","cwd":"/home/mattw/agents/workspaces/foreman"}`)
	fsys.times["/sessions/new-name.jsonl"], fsys.times["/sessions/old-name.jsonl"] = time.UnixMilli(1), time.UnixMilli(2)
	pick, err := relaunch.NewestSession("/sessions", "foreman", "/home/mattw/agents/workspaces/foreman", map[string]struct{}{"newer": {}}, relaunch.SessionDeps{FileSystem: fsys})
	if err != nil || pick == nil || pick.ID != "older" {
		t.Fatalf("NewestSession() = %#v, %v", pick, err)
	}
}

func testCoreSessionHelpersAndPresentationCase4(t *testing.T) {
	trigger := Trigger{BootID: "boot-fixture", UptimeS: 12345, ManagerUnit: "cotal-manager.service", Units: []UnitFact{{Unit: "cotal-manager.service", ActiveSince: "Sun 2026-11-01 02:52:00 UTC", InvocationID: "manager-id", NRestarts: 2, ActiveState: "active", Result: "success"}, {Unit: "zellij-server.service", ActiveSince: "Sun 2026-11-01 02:50:00 UTC", InvocationID: "zellij-id", NRestarts: 1, ActiveState: "active", Result: "success"}}, ZellijServer: "3845913:9828715"}
	for _, written := range []bool{true, false} {
		prompt := relaunchPrompt(testConfig(), trigger, "~/agents/state/wave-relaunch-manifest.json", written)
		if len(prompt) > 2000 || strings.Contains(prompt, "spawn-agent") || strings.Contains(prompt, "--fresh-session") || !strings.Contains(prompt, "Lanes relaunch from /state/wave-seats.json; do not spawn them. For a seat wave-seats parks, run wave-seats resume <name>.") || strings.Contains(prompt, "wave-relaunch-manifest.json") != written || !strings.Contains(prompt, "zellij server pid:starttime") {
			t.Fatalf("unexpected relaunch prompt: %s", prompt)
		}
	}
}

func testCoreSessionHelpersAndPresentationCase5(t *testing.T) {
	for _, tc := range []struct{ value, phrase string }{{"none", "zellij server not running"}, {"unavailable", "zellij server unreadable"}} {
		notice := liveNotice(Trigger{BootID: "boot", ZellijServer: tc.value})
		if !strings.Contains(notice, tc.phrase) || strings.Contains(notice, "zellij server "+tc.value) {
			t.Errorf("liveNotice(%q) = %q", tc.value, notice)
		}
	}
}

func testCoreSessionHelpersAndPresentationCase6(t *testing.T) {
	line := summaryLine(Outcome{Outcome: "failed", Reason: "wave-stop", ElapsedMS: 8})
	if !strings.HasPrefix(line, "<4> ") || !strings.Contains(line, failedMark) || stopPostLine("success", "") != "" || stopPostLine("exit-code", "") != "" || !strings.Contains(stopPostLine("timeout", ""), failedMark+" reason=unit-result") {
		t.Fatalf("summary=%q success=%q exit-code=%q timeout=%q", line, stopPostLine("success", ""), stopPostLine("exit-code", ""), stopPostLine("timeout", ""))
	}
}

func testCoreSessionHelpersAndPresentationCase7(t *testing.T) {
	if stopPostLine("signal", "TERM") != "" || !strings.Contains(stopPostLine("signal", "KILL"), failedMark) || !strings.Contains(stopPostLine("signal", ""), failedMark) || !strings.Contains(stopPostLine("core-dump", "SEGV"), failedMark) || !strings.Contains(stopPostLine("timeout", "TERM"), failedMark) {
		t.Fatal("unexpected stop-post classification")
	}
}

func testSeatPathsFromConfig(t *testing.T) {
	t.Helper()
	cfg := testConfig()
	cfg.SeatsPath = "/configured/seats-v2.json"
	deps, err := defaultDeps(cfg, func(key string) string {
		if key == "WAVE_SEATS_PATH" {
			return "/ignored/by/config.json"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if deps.Paths.Seats != "/configured/seats-v2.json" {
		t.Fatalf("seat path = %q, want the configured seatsPath", deps.Paths.Seats)
	}
}
