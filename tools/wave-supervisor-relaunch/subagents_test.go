package main

import (
	"context"
	"errors"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/cotalreach"
)

const finishedSessionRecord = `{"type":"custom","customType":"session_exit"}` + "\n"

func TestSubagentFinished(t *testing.T) {
	for _, tc := range []struct {
		name string
		tail string
		want bool
	}{
		{name: "session exit", tail: finishedSessionRecord, want: true},
		{name: "assistant stop", tail: `{"type":"message","message":{"role":"assistant","stopReason":"stop"}}` + "\n", want: true},
		{name: "assistant aborted", tail: `{"type":"message","message":{"role":"assistant","stopReason":"aborted"}}` + "\n", want: true},
		{name: "tool use", tail: `{"type":"message","message":{"role":"assistant","stopReason":"toolUse"}}` + "\n"},
		{name: "unknown", tail: `{"type":"new"}` + "\n"},
		{name: "partial", tail: "not json\n"},
		{name: "empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := subagentFinished([]byte(tc.tail)); got != tc.want {
				t.Fatalf("subagentFinished = %v, want %v", got, tc.want)
			}
		})
	}
}
func TestProcessLiveSubagentsSessionAge(t *testing.T) {
	for _, tc := range []struct {
		name     string
		header   string
		procStat string
		want     int
	}{
		{name: "header before process start", header: "1970-01-01T00:16:41Z", procStat: "btime 1000\n", want: 0},
		{name: "header after process start", header: "1970-01-01T00:16:43Z", procStat: "btime 1000\n", want: 1},
		{name: "same second remains live", header: "1970-01-01T00:16:42.4Z", procStat: "btime 1000\n", want: 1},
		{name: "malformed timestamp remains live", header: "invalid", procStat: "btime 1000\n", want: 1},
		{name: "unknown process start", header: "1970-01-01T00:16:41Z", want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fileSystem := newMemoryFS()
			data := `{"type":"title","title":"subagent"}` + "\n" + `{"type":"session","timestamp":"` + tc.header + `"}` + "\n" + `{"type":"tool_execution_start"}` + "\n"
			waveProcFixture(fileSystem, "123", "seat", "id", data)
			fileSystem.files["/proc/123/stat"] = []byte("123 (omp) S " + strings.Repeat("0 ", 18) + "250\n")
			if tc.procStat != "" {
				fileSystem.files["/proc/stat"] = []byte(tc.procStat)
			}
			got, err := processLiveSubagents(t.Context(), "/proc", fileSystem, "/sessions", "123")
			if err != nil || got != tc.want {
				t.Fatalf("processLiveSubagents = %d, %v; want %d", got, err, tc.want)
			}
		})
	}
}

func TestProcessStartTime(t *testing.T) {
	for _, tc := range []struct {
		name string
		stat string
		want time.Time
	}{
		{name: "btime and fractional ticks", stat: "cpu 0 0\nbtime 1000\n", want: time.Unix(1002, 500000000)},
		{name: "missing btime", stat: "cpu 0 0\n"},
		{name: "invalid btime", stat: "btime invalid\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fileSystem := newMemoryFS()
			fileSystem.files["/proc/123/stat"] = []byte("123 (omp) S " + strings.Repeat("0 ", 18) + "250\n")
			fileSystem.files["/proc/stat"] = []byte(tc.stat)
			if got := processStartTime("/proc/123", fileSystem); !got.Equal(tc.want) {
				t.Fatalf("processStartTime = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLiveSubagentsCountsRunningFiles(t *testing.T) {
	const seatFile = "/sessions/seat.jsonl"
	const child = "/sessions/seat/Child.jsonl"
	fsys := newMemoryFS()
	fsys.files[child] = []byte(`{"type":"model_usage"}` + "\n")
	if got, err := liveSubagents(t.Context(), fsys, seatFile, time.Time{}); err != nil || got != 1 {
		t.Fatalf("liveSubagents = %d, %v; want one running child", got, err)
	}
}

func TestLiveSubagentsUnreadableFailsClosed(t *testing.T) {
	const child = "/sessions/seat/Child.jsonl"
	fsys := newMemoryFS()
	fsys.files[child] = []byte(`{"type":"model_usage"}` + "\n")
	fsys.readHook = func(path string) ([]byte, error, bool) {
		if path == child {
			return nil, fs.ErrPermission, true
		}
		return nil, nil, false
	}
	if _, err := liveSubagents(t.Context(), fsys, "/sessions/seat.jsonl", time.Time{}); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("liveSubagents error = %v, want permission error", err)
	}
}

func TestStopOneSeatRefusesLiveSubagent(t *testing.T) {
	var calls int
	runner := func(context.Context, []string, string) (cotalreach.CommandResult, error) {
		calls++
		return cotalreach.CommandResult{}, nil
	}
	live := func(context.Context, string) (int, error) { return 1, nil }
	if err := stopOneSeat(t.Context(), "accounts", "main", runner, live); !errors.Is(err, errSubagentsGate) {
		t.Fatalf("stopOneSeat error = %v, want subagent gate", err)
	}
	if calls != 0 {
		t.Fatalf("stop runner calls = %d, want 0", calls)
	}
}

func TestCheckWaveSubagentsMissingCheckFailsClosed(t *testing.T) {
	if err := checkWaveSubagents(t.Context(), stopWaveDeps{}); err == nil || !strings.Contains(err.Error(), "no subagent check wired") {
		t.Fatalf("checkWaveSubagents error = %v", err)
	}
}

func TestSubagentFilesIgnoresNonSessionFiles(t *testing.T) {
	const seatFile = "/sessions/-home-example/2026_seat-id.jsonl"
	const stem = "/sessions/-home-example/2026_seat-id"
	fsys := newMemoryFS()
	fsys.files[stem+"/changes.jsonl"] = []byte(`{"type":"model_usage"}` + "\n")
	fsys.files[stem+"/task.md"] = []byte("notes")
	fsys.files[stem+"/log.txt"] = []byte("logs")
	fsys.files[stem+"/Child.jsonl"] = []byte(`{"type":"model_usage"}` + "\n")
	fsys.files[stem+"/Parent/Nested.jsonl"] = []byte(finishedSessionRecord)
	files, err := subagentFiles(fsys, stem)
	slices.Sort(files)
	want := []string{stem + "/Child.jsonl", stem + "/Parent/Nested.jsonl"}
	if err != nil || !slices.Equal(files, want) {
		t.Fatalf("subagentFiles = %v, %v; want %v", files, err, want)
	}
	if got, err := liveSubagents(t.Context(), fsys, seatFile, time.Time{}); err != nil || got != 1 {
		t.Fatalf("liveSubagents = %d, %v; want one live nested session", got, err)
	}
}

func TestTopLevelSessionFileByNameDoesNotReadContents(t *testing.T) {
	fsys := newMemoryFS()
	path := "/sessions/-home-example/2026_session-id.jsonl"
	fsys.files[path] = []byte(`{"type":"session","id":"session-id"}`)
	fsys.files["/sessions/-home-example/2026_session-id/Child.jsonl"] = []byte(finishedSessionRecord)
	fsys.readHook = func(string) ([]byte, error, bool) {
		return nil, errors.New("unexpected whole-file read"), true
	}
	got, err := topLevelSessionFile("/sessions", "session-id", fsys)
	if err != nil || got != path {
		t.Fatalf("topLevelSessionFile = %q, %v; want %q", got, err, path)
	}
}

// TestTopLevelSessionFileHeaderFallback finds a file not named by id through its prefix header.
func TestTopLevelSessionFileHeaderFallback(t *testing.T) {
	fsys := newMemoryFS()
	path := "/sessions/-home-example/accounts.jsonl"
	fsys.files[path] = []byte(`{"type":"session","id":"session-id"}` + "\n" + strings.Repeat("x", 1<<17))
	got, err := topLevelSessionFile("/sessions", "session-id", fsys)
	if err != nil || got != path || len(fsys.readBytes) != 0 {
		t.Fatalf("topLevelSessionFile = %q, %v; whole reads %v", got, err, fsys.readBytes)
	}
}

func TestLiveSubagentsReadsOnlyTails(t *testing.T) {
	const seatFile = "/sessions/seat-id.jsonl"
	fsys := newMemoryFS()
	fsys.files[seatFile] = []byte(finishedSessionRecord)
	big := strings.Repeat(`{"type":"model_usage"}`+"\n", 2*subagentTailBytes/23) + finishedSessionRecord
	fsys.files["/sessions/seat-id/Big.jsonl"] = []byte(big)
	got, err := liveSubagents(t.Context(), fsys, seatFile, time.Time{})
	if err != nil || got != 0 {
		t.Fatalf("liveSubagents = %d, %v; want 0", got, err)
	}
	if len(fsys.readBytes) != 0 {
		t.Fatalf("whole-file reads = %v, want none", fsys.readBytes)
	}
}

func TestGateSliceOmpPIDsReadErrorsFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		path    string
		readErr error
		wantErr bool
	}{
		{name: "denied cgroup", path: "/proc/123/cgroup", readErr: fs.ErrPermission, wantErr: true},
		{name: "denied cmdline", path: "/proc/123/cmdline", readErr: fs.ErrPermission, wantErr: true},
		{name: "missing cgroup means process exited", path: "/proc/123/cgroup", readErr: fs.ErrNotExist},
		{name: "missing command line means process exited", path: "/proc/123/cmdline", readErr: fs.ErrNotExist},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fsys := newMemoryFS()
			waveProcFixture(fsys, "123", "accounts", "session-id", "")
			fsys.readHook = func(path string) ([]byte, error, bool) {
				if path == tc.path {
					return nil, tc.readErr, true
				}
				return nil, nil, false
			}
			err := waveLiveSubagents(t.Context(), "/proc", fsys, "/sessions", "wave-agents.slice")
			if (err != nil) != tc.wantErr {
				t.Fatalf("waveLiveSubagents error = %v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestStopWaveRefusesSubagentStateBeforeSliceStop(t *testing.T) {
	for _, tc := range []struct {
		name      string
		check     func(context.Context, int) error
		wantSeats int
	}{
		{name: "initial check", check: func(context.Context, int) error { return errSubagentsGate }},
		{name: "after seat stops", check: func(_ context.Context, call int) error {
			if call == 2 {
				return errSubagentsGate
			}
			return nil
		}, wantSeats: 1},
		{name: "deadline expiry", check: func(ctx context.Context, _ int) error { <-ctx.Done(); return nil }},
		{name: "no check wired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checks, seats, slices := 0, 0, 0
			deps := stopWaveDeps{
				CheckDeadline: 5 * time.Millisecond,
				StopSeats:     func(context.Context) error { seats++; return nil },
				StopSlice:     func(context.Context) (bool, error) { slices++; return true, nil },
				PresenceGone:  func(context.Context) (bool, error) { return true, nil },
			}
			if tc.name != "no check wired" {
				deps.CheckSubagents = func(ctx context.Context) error {
					checks++
					return tc.check(ctx, checks)
				}
			}
			result := stopWave(t.Context(), deps)
			if result != "skipped:subagents" || seats != tc.wantSeats || slices != 0 {
				t.Fatalf("stopWave() = %q, seat stops = %d, slice stops = %d", result, seats, slices)
			}
		})
	}
}

func waveProcFixture(fileSystem *memoryFS, pid, seat, sessionID, child string) {
	base := "/proc/" + pid
	fileSystem.files[base+"/cgroup"] = []byte("0::/user.slice/wave-agents.slice/run-" + seat + ".scope\n")
	fileSystem.files[base+"/cmdline"] = []byte("omp\x00--session-id\x00" + sessionID + "\x00")
	fileSystem.files[base+"/environ"] = []byte("COTAL_AGENT_FILE=/agents/" + seat + ".md\x00HOME=/home/example\x00")
	file := "/sessions/-agents/2026_" + sessionID + ".jsonl"
	fileSystem.links[base+"/fd/3"] = file
	fileSystem.links[base+"/cwd"] = "/home/example/agents"
	fileSystem.files[file] = []byte(`{"type":"session","id":"` + sessionID + `"}` + "\n")
	if child != "" {
		fileSystem.files[strings.TrimSuffix(file, ".jsonl")+"/Sub.jsonl"] = []byte(child)
	}
}

func stopRecorder(argvs *[]string) processRunner {
	return func(_ context.Context, argv []string, _ string) (cotalreach.CommandResult, error) {
		*argvs = append(*argvs, strings.Join(argv, " "))
		return cotalreach.CommandResult{}, nil
	}
}

func TestStopOneSeatFinishedSubagentsStop(t *testing.T) {
	var argvs []string
	live := func(context.Context, string) (int, error) { return 0, nil }
	if err := stopOneSeat(t.Context(), "accounts", "main", stopRecorder(&argvs), live); err != nil {
		t.Fatalf("stopOneSeat = %v", err)
	}
	if len(argvs) != 1 {
		t.Fatalf("argv = %q, want one cotal stop", argvs)
	}
}

func TestStopManagedSeatsDeadSeatStops(t *testing.T) {
	var argvs []string
	live := liveCheckFor("/proc", newMemoryFS(), "/sessions", "wave-agents.slice")
	if err := stopManagedSeats(t.Context(), []string{"accounts"}, "main", stopRecorder(&argvs), live); err != nil {
		t.Fatalf("stopManagedSeats = %v", err)
	}
	if len(argvs) != 1 {
		t.Fatalf("argv = %q, want one cotal stop", argvs)
	}
}

func TestLiveSubagentsForUnresolvableSessionFailsClosed(t *testing.T) {
	fsys := newMemoryFS()
	waveProcFixture(fsys, "123", "accounts", "session-id", "")
	delete(fsys.files, "/sessions/-agents/2026_session-id.jsonl")
	delete(fsys.links, "/proc/123/fd/3")
	if _, err := liveSubagentsFor(t.Context(), "/proc", fsys, "/sessions", "wave-agents.slice", "accounts"); err == nil {
		t.Fatal("liveSubagentsFor accepted an unresolved seat session")
	}
}

func TestWaveLiveSubagentsRefusesUnknownAndRunningProcesses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setup   func(*memoryFS)
		wantErr bool
	}{
		{name: "clear", setup: func(fsys *memoryFS) {
			waveProcFixture(fsys, "123", "accounts", "session-id", finishedSessionRecord)
		}},
		{name: "running child", setup: func(fsys *memoryFS) {
			waveProcFixture(fsys, "123", "accounts", "session-id", `{"type":"model_usage"}`+"\n")
		}, wantErr: true},
		{name: "unmapped seat", setup: func(fsys *memoryFS) {
			waveProcFixture(fsys, "123", "accounts", "session-id", "")
			fsys.files["/proc/123/environ"] = []byte("HOME=/home/example\x00")
		}, wantErr: true},
		{name: "unreadable process", setup: func(fsys *memoryFS) {
			waveProcFixture(fsys, "123", "accounts", "session-id", "")
			fsys.readHook = func(path string) ([]byte, error, bool) {
				if path == "/proc/123/environ" {
					return nil, fs.ErrPermission, true
				}
				return nil, nil, false
			}
		}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fsys := newMemoryFS()
			tc.setup(fsys)
			err := waveLiveSubagents(t.Context(), "/proc", fsys, "/sessions", "wave-agents.slice")
			if (err != nil) != tc.wantErr {
				t.Fatalf("waveLiveSubagents error = %v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestWaveLiveSubagentsRequiresResolvedSession(t *testing.T) {
	fsys := newMemoryFS()
	fsys.files["/proc/123/cgroup"] = []byte("0::/user.slice/wave-agents.slice/run-accounts.scope\n")
	fsys.files["/proc/123/cmdline"] = []byte("omp\x00")
	fsys.files["/proc/123/environ"] = []byte("COTAL_AGENT_FILE=accounts\x00")
	if err := waveLiveSubagents(t.Context(), "/proc", fsys, "/sessions", "wave-agents.slice"); err == nil {
		t.Fatal("waveLiveSubagents accepted an unresolved process session")
	}
}

func TestWaveLiveSubagentsStaleForkIsBusy(t *testing.T) {
	fsys := newMemoryFS()
	fsys.files["/proc/300/cgroup"] = []byte("0::/user.slice/wave-agents.slice/run-accounts.scope\n")
	fsys.files["/proc/300/cmdline"] = []byte("omp\x00--fork\x00parent-1\x00")
	fsys.files["/proc/300/environ"] = []byte("COTAL_AGENT_FILE=/agents/accounts.md\x00HOME=/home/example\x00")
	fsys.files["/sessions/-agents/2026_parent-1.jsonl"] = []byte(`{"type":"session","id":"parent-1"}` + "\n")
	if err := waveLiveSubagents(t.Context(), "/proc", fsys, "/sessions", "wave-agents.slice"); err == nil {
		t.Fatal("waveLiveSubagents = nil for a stale fork guess; want busy")
	}
	if got, err := liveSubagentsFor(t.Context(), "/proc", fsys, "/sessions", "wave-agents.slice", "accounts"); err == nil {
		t.Fatalf("liveSubagentsFor = %d, nil for a stale fork guess; want an error", got)
	}
}

func TestLiveSubagentsForDeadSeatIsZero(t *testing.T) {
	if got, err := liveSubagentsFor(t.Context(), "/proc", newMemoryFS(), "/sessions", "wave-agents.slice", "accounts"); err != nil || got != 0 {
		t.Fatalf("liveSubagentsFor = %d, %v; want 0", got, err)
	}
}

func TestLiveSubagentsMissingSessionFailsClosed(t *testing.T) {
	if _, err := liveSubagents(t.Context(), newMemoryFS(), "", time.Time{}); err == nil {
		t.Fatal("liveSubagents accepted an empty session path")
	}
}

func TestLiveSubagentsCancellationIsReturned(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	fsys := newMemoryFS()
	fsys.files["/sessions/seat/Child.jsonl"] = []byte(`{"type":"model_usage"}` + "\n")
	if _, err := liveSubagents(ctx, fsys, "/sessions/seat.jsonl", time.Time{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("liveSubagents error = %v, want cancellation", err)
	}
}
