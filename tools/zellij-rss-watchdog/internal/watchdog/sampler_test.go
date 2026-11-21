package watchdog

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/zellij-rss-watchdog/internal/capture"
	"github.com/RigelBuild/nebula/tools/zellij-rss-watchdog/internal/command"
	"github.com/RigelBuild/nebula/tools/zellij-rss-watchdog/internal/seam"
)

const (
	statePath      = "/captures/watch-state.json"
	restartStatus  = "VmRSS:\t11000000 kB\nVmSwap:\t0 kB\n"
	restartBinPath = "/example/bin/relaunch"
)

// readStateFile parses the watch-state.json the fake wrote; a missing file fails the test.
func readStateFile(t *testing.T, io *fakeIO) watchState {
	t.Helper()
	text, ok := io.files[statePath]
	if !ok {
		t.Fatalf("%s was never written", statePath)
	}
	return readStateText(t, text)
}

// setClock pins the fake clock to ms for every Now call.
func setClock(io *fakeIO, ms int64) {
	io.clockFn = func() int64 { return ms }
}

// statusFake serves /proc/123/status from a mutable status body and passes state files through.
func statusFake(io *fakeIO, status *string) {
	io.readTextFn = func(path string) (string, error) {
		if path == "/proc/123/status" {
			return *status, nil
		}
		return io.files[path], nil
	}
}

func TestFailedStatusReadOnSameServerKeepsPersistedRestartTimestamps(t *testing.T) {
	io := newFakeIO()
	statusOK := true
	io.readTextFn = func(path string) (string, error) {
		if path != "/proc/123/status" {
			return io.files[path], nil
		}
		if !statusOK {
			return "", errors.New("EACCES")
		}
		return restartStatus, nil
	}
	var calls [][]string
	io.runFn = func(_ context.Context, _ string, args []string, _ time.Duration) (command.Result, error) {
		calls = append(calls, append([]string(nil), args...))
		return command.Result{}, nil
	}
	ctx := context.Background()
	setClock(io, 0)
	must(t, sampleOnce(ctx, io))
	setClock(io, rssRestartDelayMS)
	must(t, sampleOnce(ctx, io))
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	before := readStateFile(t, io)
	if before.OverSince == nil || *before.OverSince != 0 {
		t.Fatalf("overSince = %v, want 0", before.OverSince)
	}
	if before.LastRequestAt == nil || *before.LastRequestAt != rssRestartDelayMS {
		t.Fatalf("lastRequestAt = %v, want %d", before.LastRequestAt, rssRestartDelayMS)
	}
	statusOK = false
	setClock(io, rssRestartDelayMS+rssRestartRetryMS)
	must(t, sampleOnce(ctx, io))
	after := readStateFile(t, io)
	if *after.OverSince != *before.OverSince || *after.LastRequestAt != *before.LastRequestAt {
		t.Fatal("restart timestamps changed after a failed status read")
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
}

func TestRequestsRestartAfterTenMinutesAndRecordsSuccess(t *testing.T) {
	io := newFakeIO()
	status := restartStatus
	statusFake(io, &status)
	var calls [][]string
	io.runFn = func(_ context.Context, name string, args []string, _ time.Duration) (command.Result, error) {
		if name != "runuser" {
			return command.Result{}, fmt.Errorf("unexpected command: %s", name)
		}
		calls = append(calls, append([]string(nil), args...))
		return command.Result{}, nil
	}
	ctx := context.Background()
	setClock(io, 0)
	must(t, sampleOnce(ctx, io))
	setClock(io, rssRestartDelayMS-sampleIntervalMS)
	must(t, sampleOnce(ctx, io))
	if len(calls) != 0 {
		t.Fatalf("calls before delay = %d, want 0", len(calls))
	}
	setClock(io, rssRestartDelayMS)
	must(t, sampleOnce(ctx, io))
	want := []string{
		"-u", "example-user", "--",
		"timeout", "--kill-after=2", "25",
		"env", "XDG_RUNTIME_DIR=/run/user/1000",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus",
		restartBinPath,
		"request-zellij-restart", "--reason", "rss", "--rss", "123:456",
	}
	if len(calls) != 1 || !reflect.DeepEqual(calls[0], want) {
		t.Fatalf("calls = %v, want [%v]", calls, want)
	}
	if !io.logged("info", "event=restart_requested identity=123:456") {
		t.Fatal("restart_requested not logged")
	}
	if got := readStateFile(t, io); got.LastRequestAt == nil || *got.LastRequestAt != rssRestartDelayMS {
		t.Fatalf("lastRequestAt = %v, want %d", got.LastRequestAt, rssRestartDelayMS)
	}
	setClock(io, rssRestartDelayMS+rssRestartRetryMS-1)
	must(t, sampleOnce(ctx, io))
	if len(calls) != 1 {
		t.Fatalf("calls during retry window = %d, want 1", len(calls))
	}
	setClock(io, rssRestartDelayMS+rssRestartRetryMS)
	must(t, sampleOnce(ctx, io))
	if len(calls) != 2 {
		t.Fatalf("calls after retry window = %d, want 2", len(calls))
	}
}

func TestQueuedRequestDoesNotRepeatDuringThirtyMinuteCooldown(t *testing.T) {
	io := newFakeIO()
	status := "VmRSS:\t11000000 kB\n"
	statusFake(io, &status)
	attempts := 0
	io.runFn = func(context.Context, string, []string, time.Duration) (command.Result, error) {
		attempts++
		return command.Result{}, nil
	}
	ctx := context.Background()
	setClock(io, 0)
	must(t, sampleOnce(ctx, io))
	setClock(io, rssRestartDelayMS)
	must(t, sampleOnce(ctx, io))
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	setClock(io, rssRestartDelayMS+rssRestartRetryMS-sampleIntervalMS)
	must(t, sampleOnce(ctx, io))
	if attempts != 1 {
		t.Fatalf("attempts during cooldown = %d, want 1", attempts)
	}
	setClock(io, rssRestartDelayMS+rssRestartRetryMS)
	must(t, sampleOnce(ctx, io))
	if attempts != 2 {
		t.Fatalf("attempts after cooldown = %d, want 2", attempts)
	}
}

func TestFailedRestartRequestLeavesTimeUnsetAndRetriesNextSample(t *testing.T) {
	io := newFakeIO()
	status := "VmRSS:\t11000000 kB\n"
	statusFake(io, &status)
	attempts := 0
	io.runFn = func(context.Context, string, []string, time.Duration) (command.Result, error) {
		attempts++
		if attempts == 1 {
			return command.Result{}, errors.New("request exited with status 1")
		}
		return command.Result{}, nil
	}
	ctx := context.Background()
	setClock(io, 0)
	must(t, sampleOnce(ctx, io))
	setClock(io, rssRestartDelayMS)
	must(t, sampleOnce(ctx, io))
	if !io.logged("error", "event=restart_request_failed") {
		t.Fatal("restart_request_failed not logged")
	}
	if got := readStateFile(t, io); got.LastRequestAt != nil {
		t.Fatalf("lastRequestAt = %d, want unset", *got.LastRequestAt)
	}
	must(t, sampleOnce(ctx, io))
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestSamplePersistsBaselineAndCaptureMarkAndEscalatesAfterReload(t *testing.T) {
	io := newFakeIO()
	rss := gib
	statusReads := 0
	captures := 0
	io.readTextFn = func(path string) (string, error) {
		if path == "/proc/123/status" {
			statusReads++
			return fmt.Sprintf("Name:\tzellij\nVmRSS:\t%d kB\nVmSwap:\t0 kB\n", rss/1024), nil
		}
		return io.files[path], nil
	}
	io.writeTextFn = func(path, text string) error {
		io.files[path] = text
		if strings.Contains(path, "/sample-rss.txt") {
			captures++
		}
		return nil
	}
	ctx := context.Background()
	must(t, sampleOnce(ctx, io))
	rss = gibs(2.6)
	must(t, sampleOnce(ctx, io))
	if captures != 1 {
		t.Fatalf("captures = %d, want 1", captures)
	}
	// Two sampleOnce calls plus captureServer's own status read.
	if statusReads != 3 {
		t.Fatalf("statusReads = %d, want 3", statusReads)
	}
	state := readStateFile(t, io)
	if state.Armed {
		t.Fatal("armed after capture")
	}
	wantMark := (gibs(2.6) / 1024) * 1024
	if state.LastCaptureBytes == nil || *state.LastCaptureBytes != wantMark {
		t.Fatalf("lastCaptureBytes = %v, want %d", state.LastCaptureBytes, wantMark)
	}
	rss = 6 * gib
	must(t, sampleOnce(ctx, io))
	if captures != 1 {
		t.Fatalf("captures at 6 GiB = %d, want 1", captures)
	}
	rss = gibs(6.7)
	must(t, sampleOnce(ctx, io))
	if captures != 2 {
		t.Fatalf("captures after escalation = %d, want 2", captures)
	}
}

func TestChangedIdentityResetsRestartTimingBeforeFailedStatusRead(t *testing.T) {
	io := newFakeIO()
	io.identityFn = func() string { return "124:457" }
	io.readTextFn = func(path string) (string, error) {
		if path == "/proc/124/status" {
			return "", errors.New("status unreadable")
		}
		return io.files[path], nil
	}
	io.files[statePath] = `{"identity":"123:456","armed":true,"observations":[],"overSince":1,"lastRequestAt":2}`
	must(t, sampleOnce(context.Background(), io))
	state := readStateFile(t, io)
	if state.Identity != "124:457" || !state.Armed || len(state.Observations) != 0 {
		t.Fatalf("state = %+v, want identity 124:457, armed, no observations", state)
	}
	if state.OverSince != nil {
		t.Fatalf("overSince = %d, want unset", *state.OverSince)
	}
	if state.LastRequestAt != nil {
		t.Fatalf("lastRequestAt = %d, want unset", *state.LastRequestAt)
	}
}

func TestRetainsOnlyNewestThreeCaptureDirectories(t *testing.T) {
	got := capture.PruneCaptures([]string{"2026-01-01", "2026-01-05", "2026-01-03", "2026-01-02", "2026-01-04"})
	if want := []string{"2026-01-02", "2026-01-01"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("removed = %v, want %v", got, want)
	}
}

func TestFailingCaptureStepIsRecordedAndDoesNotAbortOthers(t *testing.T) {
	io := newFakeIO()
	var capturedFiles []string
	errorsText := ""
	io.writeTextFn = func(path, text string) error {
		capturedFiles = append(capturedFiles, path)
		if strings.HasSuffix(path, "status.txt") {
			return errors.New("status read-only")
		}
		if strings.HasSuffix(path, "errors.txt") {
			errorsText = text
		}
		io.files[path] = text
		return nil
	}
	must(t, capture.CaptureServer(context.Background(), io, "123:456", 2*gib))
	if !strings.Contains(errorsText, "status: status read-only") {
		t.Fatalf("errors.txt = %q, want status failure", errorsText)
	}
	for _, suffix := range []string{"peers.txt", "unit-journal.txt", "zellij-clients.txt"} {
		if !anyHasSuffix(capturedFiles, suffix) {
			t.Errorf("%s not written after an earlier step failed", suffix)
		}
	}
}

func anyHasSuffix(values []string, suffix string) bool {
	for _, value := range values {
		if strings.HasSuffix(value, suffix) {
			return true
		}
	}
	return false
}

func TestUserLogsSelectNewestFilesFirstUnderAggregateCap(t *testing.T) {
	const mib = int64(1024 * 1024)
	var reads []string
	var counts []int64
	entries := []seam.UserLogEntry{
		{Name: "oldest.log", Modified: 1, Size: 30 * mib},
		{Name: "older.log", Modified: 2, Size: 12 * mib},
		{Name: "newest.log", Modified: 3, Size: 12 * mib},
	}
	result := readUserLogs(capture.MaxLogBytes, entries, func(name string, count int64) ([]byte, error) {
		reads = append(reads, name)
		counts = append(counts, count)
		return make([]byte, count), nil
	})
	if want := []string{"newest.log", "older.log"}; !reflect.DeepEqual(reads, want) {
		t.Fatalf("reads = %v, want %v", reads, want)
	}
	if want := []int64{12 * mib, 8 * mib}; !reflect.DeepEqual(counts, want) {
		t.Fatalf("counts = %v, want %v", counts, want)
	}
	names := make([]string, 0, len(result.Files))
	total := 0
	for _, file := range result.Files {
		names = append(names, file.Name)
		total += len(file.Contents)
	}
	if want := []string{"newest.log", "older.log"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("files = %v, want %v", names, want)
	}
	if int64(total) != capture.MaxLogBytes {
		t.Fatalf("total bytes = %d, want %d", total, capture.MaxLogBytes)
	}
}

func TestUserOwnedCommandsUseExactArgvAndEnvironment(t *testing.T) {
	io := newFakeIO()
	var calls [][]string
	io.runFn = func(_ context.Context, name string, args []string, _ time.Duration) (command.Result, error) {
		if name == "ss" {
			return command.Result{}, nil
		}
		if name != "runuser" {
			return command.Result{}, fmt.Errorf("unexpected command: %s", name)
		}
		calls = append(calls, append([]string(nil), args...))
		if containsArg(args, "journalctl") {
			return command.Result{Stdout: "journal"}, nil
		}
		return command.Result{Stdout: "clients"}, nil
	}
	must(t, capture.CaptureServer(context.Background(), io, "123:456", 2*gib))
	want := [][]string{
		{
			"-u", "example-user", "--", "timeout", "--kill-after=2", "25",
			"env", "XDG_RUNTIME_DIR=/run/user/1000",
			"journalctl", "--user", "-u", testJournalUnit, "--since", "-10min", "--no-pager",
		},
		{
			"-u", "example-user", "--", "timeout", "--kill-after=2", "25",
			"env", "TMPDIR=/run/user/1000/zellij-rss-watchdog", "ZELLIJ_SESSION_NAME=example-session", "XDG_RUNTIME_DIR=/run/user/1000",
			"zellij", "--session", "example-session", "action", "list-clients",
		},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func containsArg(args []string, target string) bool {
	return slices.Contains(args, target)
}
