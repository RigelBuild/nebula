package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/cotalreach"
	"github.com/RigelBuild/nebula/tools/go-lib/relaunch"
)

func TestRelaunchCommandCLI(t *testing.T) {
	for _, tc := range []struct {
		name         string
		args         []string
		wantReason   string
		wantIdentity string
		valid        bool
	}{
		{name: "reason and rss", args: []string{"--reason", "x", "--rss", "0005:01"}, wantReason: "x", wantIdentity: "0005:01", valid: true},
		{name: "rss and reason", args: []string{"--rss", "0005:01", "--reason", "x"}, wantReason: "x", wantIdentity: "0005:01", valid: true},
		{name: "reason remains free text", args: []string{"--reason", "x with --flags"}, wantReason: "x with --flags", valid: true},
		{name: "missing reason", args: []string{"--rss", "5:1"}},
		{name: "duplicate reason", args: []string{"--reason", "x", "--reason", "y"}},
		{name: "duplicate rss", args: []string{"--reason", "x", "--rss", "5:1", "--rss", "5:1"}},
		{name: "missing rss value", args: []string{"--reason", "x", "--rss"}},
		{name: "invalid rss value", args: []string{"--reason", "x", "--rss", "5:x"}},
		{name: "unknown flag", args: []string{"--reason", "x", "--bad"}},
		{name: "blank reason", args: []string{"--reason", "  "}},
		{name: "flag instead of reason", args: []string{"--reason", "--rss"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason, identity, valid := parseRestartArgs(tc.args)
			if valid != tc.valid || reason != tc.wantReason || identity != tc.wantIdentity {
				t.Fatalf("parseRestartArgs(%q) = (%q, %q, %t), want (%q, %q, %t)", tc.args, reason, identity, valid, tc.wantReason, tc.wantIdentity, tc.valid)
			}
		})
	}
	t.Run("restart flags preserve free text and parse in either order", testRelaunchCommandCLIRestartFlagsPreserveFreeTextAnd)
	t.Run("hold default and invalid arguments", testRelaunchCommandCLIHoldDefaultAndInvalidArguments)
	t.Run("request stores restart data and starts service", testRelaunchCommandCLIRequestStoresRestartDataAndStarts)
	t.Run("restart defaults cause to wedge and removes healthy request", testRelaunchCommandCLIRestartDefaultsCauseToWedgeAnd)
	for _, args := range [][]string{
		{"request-zellij-restart"},
		{"request-zellij-restart", "--rss", "500:1"},
		{"request-zellij-restart", "--reason", "x", "--rss"},
		{"request-zellij-restart", "--reason", "x", "--rss", "500:1", "--rss", "500:1"},
		{"request-zellij-restart", "--reason", "x", "--reason", "y"},
		{"request-zellij-restart", "--reason", "   "},
		{"request-zellij-restart", "--reason", "x", "unexpected"},
	} {
		t.Run(strings.Join(args, "/"), func(t *testing.T) {
			if got := runCommandForTest(t.Context(), args, newRunHarness().deps); got != 2 {
				t.Fatalf("command(%v)=%d", args, got)
			}
		})
	}
	t.Run("restart service failure removes request", testRelaunchCommandCLIRestartServiceFailureRemovesRequest)
	t.Run("restart startup error cleans request and keeps error status", testRelaunchCommandCLIRestartStartupErrorCleansRequestAnd)
	t.Run("failed request cleanup is returned with startup error", testRelaunchCommandCLIFailedRequestCleanupIsReturnedWith)
	t.Run("usage writer failure exits as error", testRelaunchCommandCLIUsageWriterFailureExitsAsError)
	t.Run("run and stop-post propagate summary writer failure", testRelaunchCommandCLIRunAndStopPostPropagateSummary)
	t.Run("run summary returns failure status", testRelaunchCommandCLIRunSummaryReturnsFailureStatus)
	t.Run("run writes one summary and returns status only for failure", testRelaunchCommandCLIRunWritesOneSummaryAndReturns)
	t.Run("stop-post suppresses normal service result", testRelaunchCommandCLIStopPostSuppressesNormalServiceResult)
	t.Run("release removes an active hold and accepts only no arguments", testRelaunchCommandCLIReleaseRemovesAnActiveHoldAnd)
	t.Run("stop-post reports timeout and ignores TERM", testRelaunchCommandCLIStopPostReportsTimeoutAndIgnores)
}

func testRelaunchCommandCLIRunWritesOneSummaryAndReturns(t *testing.T) {
	t.Helper()
	for _, tc := range []struct {
		name       string
		missing    bool
		wantStatus int
	}{
		{name: "successful skipped outcome", wantStatus: 0},
		{name: "failed no-session outcome", missing: true, wantStatus: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRunHarness()
			h.ps = testLivePS
			lines := []string{}
			h.deps.WriteLog = func(line string) error { lines = append(lines, line); return nil }
			if tc.missing {
				h.fs.files = map[string][]byte{}
				h.ps = ""
			}
			if got := runCommandForTest(t.Context(), []string{"run"}, h.deps); got != tc.wantStatus || len(lines) != 1 {
				t.Fatalf("status=%d lines=%v", got, lines)
			}
			if tc.missing && !strings.Contains(lines[0], "outcome=failed reason=no-session") {
				t.Fatalf("failure summary=%q", lines[0])
			}
		})
	}
}

func testRelaunchCommandCLIHoldDefaultAndInvalidArguments(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	if got := runCommandForTest(t.Context(), []string{"hold", "--reason", "operator"}, h.deps); got != 0 {
		t.Fatalf("command status=%d", got)
	}
	hold, expired, err := relaunch.ReadHold(h.deps.Paths.Hold, h.time, h.fs)
	if err != nil || expired || hold == nil || hold.Until != h.time.Add(30*time.Minute).Format("2006-01-02T15:04:05.000Z") {
		t.Fatalf("hold=%#v expired=%t err=%v", hold, expired, err)
	}
	if got := runCommandForTest(t.Context(), []string{"hold", "--reason", "operator", "--minutes", "120"}, h.deps); got != 0 {
		t.Fatalf("maximum hold status=%d", got)
	}
	if got := runCommandForTest(t.Context(), []string{"hold", "--reason", "operator", "--minutes", "121"}, h.deps); got != 2 {
		t.Fatalf("invalid hold=%d", got)
	}
	if got := runCommandForTest(t.Context(), []string{"hold", "--reason", " "}, h.deps); got != 2 {
		t.Fatalf("blank hold=%d", got)
	}
}

func testRelaunchCommandCLIFailedRequestCleanupIsReturnedWith(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	startErr := errors.New("start failed")
	removeErr := errors.New("remove failed")
	h.deps.FS = failingRemoveFS{FileSystem: h.fs, path: h.deps.Paths.RestartRequest, err: removeErr}
	h.deps.StartRelaunch = func(context.Context) (bool, error) { return false, startErr }
	root := newRootCommand(t.Context(), h.deps, io.Discard, io.Discard)
	root.SetArgs([]string{"request-zellij-restart", "--reason", "test"})
	err := root.ExecuteContext(context.Background())
	if !errors.Is(err, startErr) || !errors.Is(err, removeErr) {
		t.Fatalf("error=%v", err)
	}
}

func testRelaunchCommandCLIUsageWriterFailureExitsAsError(t *testing.T) {
	t.Helper()
	root := newRootCommand(t.Context(), newRunHarness().deps, io.Discard, errWriter{})
	root.SetArgs([]string{"request-zellij-restart", "--unknown"})
	if err := root.ExecuteContext(context.Background()); err == nil || !strings.Contains(err.Error(), "write usage") {
		t.Fatalf("error=%v", err)
	}
}

func testRelaunchCommandCLIRunAndStopPostPropagateSummary(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.ps = testLivePS
	h.deps.WriteLog = func(string) error { return errors.New("output failed") }
	if got := runCommandForTest(t.Context(), []string{"run"}, h.deps); got != 1 {
		t.Fatalf("run status=%d", got)
	}
	h.deps.ServiceResult = "timeout"
	if got := runCommandForTest(t.Context(), []string{"stop-post"}, h.deps); got != 1 {
		t.Fatalf("stop-post status=%d", got)
	}
}

func testRelaunchCommandCLIRunSummaryReturnsFailureStatus(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.fs.files = map[string][]byte{}
	if got := runCommandForTest(t.Context(), []string{"run"}, h.deps); got != 1 {
		t.Fatalf("command status=%d", got)
	}
}

func testRelaunchCommandCLIStopPostSuppressesNormalServiceResult(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.deps.ServiceResult = "success"
	lines := 0
	h.deps.WriteLog = func(string) error { lines++; return nil }
	if got := runCommandForTest(t.Context(), []string{"stop-post"}, h.deps); got != 0 || lines != 0 {
		t.Fatalf("status=%d lines=%d", got, lines)
	}
}

func testRelaunchCommandCLIReleaseRemovesAnActiveHoldAnd(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	h.write(h.deps.Paths.Hold, fmt.Sprintf(`{"until":%q,"reason":"operator"}`, h.time.Add(time.Hour).Format(time.RFC3339Nano)))
	if got := runCommandForTest(t.Context(), []string{"release"}, h.deps); got != 0 {
		t.Fatalf("release status=%d", got)
	}
	if _, exists := h.fs.files[h.deps.Paths.Hold]; exists {
		t.Fatal("hold remains after release")
	}
	if got := runCommandForTest(t.Context(), []string{"release", "extra"}, h.deps); got != 2 {
		t.Fatalf("release with extra arg status=%d", got)
	}
}

func testRelaunchCommandCLIStopPostReportsTimeoutAndIgnores(t *testing.T) {
	t.Helper()
	h := newRunHarness()
	lines := make([]string, 0, 1)
	h.deps.WriteLog = func(line string) error { lines = append(lines, line); return nil }
	h.deps.ServiceResult = "timeout"
	if got := runCommandForTest(t.Context(), []string{"stop-post"}, h.deps); got != 0 || len(lines) != 1 || !strings.Contains(lines[0], "outcome=failed") {
		t.Fatalf("status=%d lines=%v", got, lines)
	}
	h.deps.ServiceResult, h.deps.ServiceExitStatus = "signal", "TERM"
	if got := runCommandForTest(t.Context(), []string{"stop-post"}, h.deps); got != 0 || len(lines) != 1 {
		t.Fatalf("status=%d lines=%v", got, lines)
	}
}

// seatStepHarness is a run harness whose runner records argv as a fake cotal, wave-seats and
// wave-seat-holder would see it, with one idle non-root seat over the threshold.
func seatStepHarness(t *testing.T, stopExit int) (*runHarness, *[]string, *[]string) {
	t.Helper()
	h := newRunHarness()
	h.ps = testLivePS
	argvs := []string{}
	lines := []string{}
	h.deps.WriteLog = func(line string) error { lines = append(lines, line); return nil }
	h.deps.Paths.Cgroup = "/cg"
	h.write("/cg/user.slice/wave-agents.slice/run-accounts.scope/memory.stat", "anon 2147483648\n")
	h.deps.SeatThreshold = "1"
	h.deps.Runner = func(_ context.Context, argv []string, _ string) (cotalreach.CommandResult, error) {
		argvs = append(argvs, strings.Join(argv, " "))
		if argv[0] == cotalCommand && argv[1] == "stop" {
			return cotalreach.CommandResult{ExitCode: stopExit}, nil
		}
		return cotalreach.CommandResult{}, nil
	}
	h.deps.Holders = func(context.Context) (string, error) { return "", nil }
	stopped := false
	h.deps.PS = func(context.Context) (string, error) {
		for _, argv := range argvs {
			stopped = stopped || strings.HasPrefix(argv, "cotal stop")
		}
		if stopped {
			return testPSEnvelope(testLivePS), nil
		}
		return testPSEnvelope(testLivePS + `,{"name":"accounts","mesh":"idle","meshView":"current","presenceIdentityVerified":true,"background":"idle"}`), nil
	}
	h.write("/proc/222/cmdline", "omp\x00--session-id\x00accounts-id\x00")
	h.write("/proc/222/environ", "COTAL_AGENT_FILE=/agents/accounts.md\x00")
	h.fs.links["/proc/222/fd/1"] = "/sessions/seat/session_accounts-id.jsonl"
	h.write("/sessions/seat/session_accounts-id.jsonl", `{"type":"session","id":"accounts-id"}`)
	h.write("/proc/222/cgroup", "0::/user.slice/wave-agents.slice/run-accounts.scope\n")
	return h, &argvs, &lines
}

func TestRunSeatStepArgvAndExitCode(t *testing.T) {
	h, argvs, lines := seatStepHarness(t, 0)
	// The first run notifies (armed), which touches the root, so the seat step must not run.
	if got := runCommandForTest(t.Context(), []string{"run"}, h.deps); got != 0 || len(*argvs) != 0 {
		t.Fatalf("armed run: status = %d argv = %q", got, *argvs)
	}
	// The second run is unchanged, a no-op, so the seat step follows.
	if got := runCommandForTest(t.Context(), []string{"run"}, h.deps); got != 0 {
		t.Fatalf("status = %d, lines %q", got, *lines)
	}
	want := "cotal stop --name accounts --space main|wave-seats restart accounts"
	if got := strings.Join(*argvs, "|"); got != want {
		t.Fatalf("argv = %q, want %q (lines %q)", got, want, *lines)
	}
}

func TestRunSeatStepFailedStopKeepsExitCode(t *testing.T) {
	h, argvs, lines := seatStepHarness(t, 1)
	if got := runCommandForTest(t.Context(), []string{"run"}, h.deps); got != 0 {
		t.Fatalf("armed run status = %d", got)
	}
	if got := runCommandForTest(t.Context(), []string{"run"}, h.deps); got != 0 {
		t.Fatalf("status = %d, want 0: the seat step never changes run's exit code", got)
	}
	if got := strings.Join(*argvs, "|"); got != "cotal stop --name accounts --space main" {
		t.Fatalf("argv = %q, want only the stop", got)
	}
	if last := (*lines)[len(*lines)-1]; !strings.Contains(last, "outcome=failed seat=accounts") {
		t.Fatalf("last line = %q", last)
	}
}
