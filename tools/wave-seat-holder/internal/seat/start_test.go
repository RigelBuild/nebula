package seat

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
)

func TestStartDarwinStartsSeatsThroughWaveHost(t *testing.T) {
	fake := fakeDeps(fakeOptions{})
	fake.deps.Platform = "darwin"
	fake.deps.Getenv = lookupMap(map[string]string{"HOME": "/home/test"})
	fake.deps.Spawn = func(_ context.Context, command string, args []string, inherit bool) (CommandResult, error) {
		fake.calls = append(fake.calls, recordedCall{command: command, args: args, inherit: inherit})
		return CommandResult{Stdout: "123 789\n"}, nil
	}
	assertDeepEqual(t, Run(context.Background(), []string{"start", "--name", "seat", "--", "omp"}, fake.deps), 0)
	assertDeepEqual(t, fake.calls, []recordedCall{{
		command: waveHostCommand,
		args: []string{
			"seat-start", "--name", "seat",
			"--socket", "/home/test/agents/run/seats/seat.sock",
			"--conf", testDarwinSeatConf,
			"--cwd", "/srv/agent",
			"--", "omp",
		},
	}})
	assertDeepEqual(t, fake.output, []string{"123 789\n"})
}

func TestStartDarwinRejectsFailuresWithTheTSReason(t *testing.T) {
	tests := []struct {
		name    string
		home    string
		code    int
		stdout  string
		message string
	}{
		{name: "missing HOME", home: "", code: 0, stdout: "123 789", message: "HOME must be set"},
		{name: "host failure", home: "/home/test", code: 1, stdout: "", message: "host failed"},
		{name: "bad pids", home: "/home/test", code: 0, stdout: "bad", message: "invalid process ids"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := fakeDeps(fakeOptions{})
			fake.deps.Platform = "darwin"
			fake.deps.Getenv = lookupMap(map[string]string{"HOME": test.home})
			fake.deps.Spawn = func(_ context.Context, _ string, _ []string, _ bool) (CommandResult, error) {
				return CommandResult{Code: test.code, Stdout: test.stdout, Stderr: "host failed"}, nil
			}
			assertDeepEqual(t, Run(context.Background(), []string{"start", "--name", "seat", "--", "omp"}, fake.deps), 1)
			if !strings.Contains(strings.Join(fake.errors, ""), test.message) {
				t.Fatalf("errors = %q, want one containing %q", fake.errors, test.message)
			}
		})
	}
}

func TestStartUsesSystemdAndTmuxArgvAndReportsBothPIDs(t *testing.T) {
	fake := fakeDeps(fakeOptions{})
	assertDeepEqual(t, Run(context.Background(), []string{"start", "--name", "seat", "--", "omp", "--agent", "one"}, fake.deps), 0)
	assertDeepEqual(t, fake.errors, []string(nil))
	assertDeepEqual(t, fake.output, []string{"123 789\n"})
	assertDeepEqual(t, fake.mkdirs, []recordedMkdir{{path: "/run/test-seats", perm: 0o700}})
	assertDeepEqual(t, fake.calls, []recordedCall{
		{command: systemctlCommand, args: []string{"--user", "show", "-p", "LoadState", "test-seat-seat"}},
		{command: systemdRunCommand, args: []string{
			"--user", "--unit=test-seat-seat", "--slice=test-agents.slice",
			"-p", "Type=exec", "--collect", "--",
			testTmux, "-S", testSocket, "-f", testSeatConf, "-D",
		}},
		{command: testTmux, args: []string{"-N", "-S", testSocket, "set-option", "-g", "remain-on-exit", "on"}},
		{command: testTmux, args: []string{
			"-N", "-S", testSocket, "new-session", "-d", "-s", "seat",
			"-x", "200", "-y", "50", "-c", "/srv/agent", "--", "omp", "--agent", "one",
		}},
		{command: systemctlCommand, args: []string{"--user", "show", "-p", "MainPID", "--value", "test-seat-seat"}},
		{command: testTmux, args: []string{"-N", "-S", testSocket, "display", "-p", "-t", "seat", "#{pane_dead}"}},
		{command: testTmux, args: []string{"-N", "-S", testSocket, "display", "-p", "-t", "seat", "#{pane_pid}"}},
		{command: testTmux, args: []string{"-N", "-S", testSocket, "set-option", "-g", "remain-on-exit", "off"}},
		{command: testTmux, args: []string{"-N", "-S", testSocket, "display", "-p", "-t", "seat", "#{pane_dead}"}},
	})
}

func TestStartReturnsFourWhenTheUnitIsLoaded(t *testing.T) {
	fake := fakeDeps(fakeOptions{loadState: "loaded"})
	assertDeepEqual(t, Run(context.Background(), []string{"start", "--name", "seat", "--", "omp"}, fake.deps), 4)
	assertDeepEqual(t, recordedCommands(fake.calls), []string{systemctlCommand})
	assertContainsLine(t, fake.errors, "already live")
}

func TestStartReturnsFourWhenTheSocketAcceptsAConnection(t *testing.T) {
	fake := fakeDeps(fakeOptions{liveSocketPaths: []string{testSocket}})
	assertDeepEqual(t, Run(context.Background(), []string{"start", "--name", "seat", "--", "omp"}, fake.deps), 4)
	assertDeepEqual(t, fake.calls, []recordedCall(nil))
	assertContainsLine(t, fake.errors, "already live")
}

func TestStartReturnsOneWithReasonWhenSystemdReportsMainPIDZero(t *testing.T) {
	fake := fakeDeps(fakeOptions{mainPID: "0"})
	assertDeepEqual(t, Run(context.Background(), []string{"start", "--name", "seat", "--", "omp"}, fake.deps), 1)
	assertContainsLine(t, fake.errors, "MainPID")
}

func TestStartFailedNewSessionStopsTheUnit(t *testing.T) {
	fake := fakeDeps(fakeOptions{newSessionCode: 1})
	assertDeepEqual(t, Run(context.Background(), []string{"start", "--name", "seat", "--", "omp"}, fake.deps), 1)
	assertContainsLine(t, fake.errors, "new-session failed")
	assertDeepEqual(t, fake.calls[len(fake.calls)-1], recordedCall{
		command: systemctlCommand,
		args:    []string{"--user", "stop", "test-seat-seat"},
	})
}

func TestStartWaitsForTheLauncherToForkTheAgentBeforeReporting(t *testing.T) {
	fake := fakeDeps(fakeOptions{children: []string{"", "", "789\n"}})
	assertDeepEqual(t, Run(context.Background(), []string{"start", "--name", "seat", "--", "omp"}, fake.deps), 0)
	assertDeepEqual(t, fake.output, []string{"123 789\n"})
}

func TestStartReturnsOneAndStopsTheUnitWhenTheAgentNeverStarts(t *testing.T) {
	fake := fakeDeps(fakeOptions{children: []string{""}})
	assertDeepEqual(t, Run(context.Background(), []string{"start", "--name", "seat", "--", "omp"}, fake.deps), 1)
	assertDeepEqual(t, fake.output, []string(nil))
	assertContainsLine(t, fake.errors, "process ids")
	assertDeepEqual(t, fake.calls[len(fake.calls)-1].args, []string{"--user", "stop", "test-seat-seat"})
}

func TestStartRefusesBeforeStartingAUnitWhenTmuxIsNotOnPath(t *testing.T) {
	empty := ""
	fake := fakeDeps(fakeOptions{tmuxPath: &empty})
	assertDeepEqual(t, Run(context.Background(), []string{"start", "--name", "seat", "--", "omp"}, fake.deps), 1)
	assertDeepEqual(t, recordedCommands(fake.calls), []string{systemctlCommand})
	assertContainsLine(t, fake.errors, "tmux is not on PATH")
}

func TestStartTimesOutAndStopsTheUnitWhenTheSocketNeverGoesLive(t *testing.T) {
	fake := fakeDeps(fakeOptions{})
	fake.deps.Files.Connect = func(context.Context, string) bool { return false }
	assertDeepEqual(t, Run(context.Background(), []string{"start", "--name", "seat", "--", "omp"}, fake.deps), 1)
	assertContainsLine(t, fake.errors, "timed out waiting for socket")
	assertDeepEqual(t, fake.calls[len(fake.calls)-1], recordedCall{
		command: systemctlCommand,
		args:    []string{"--user", "stop", "test-seat-seat"},
	})
}

// recordedCommands returns the command of each call, in order.
func recordedCommands(calls []recordedCall) []string {
	commands := make([]string, 0, len(calls))
	for _, call := range calls {
		commands = append(commands, call.command)
	}
	return commands
}

// assertContainsLine fails unless one of lines mentions want.
func assertContainsLine(t *testing.T, lines []string, want string) {
	t.Helper()
	if !strings.Contains(strings.Join(lines, " "), want) {
		t.Fatalf("lines = %q, want one containing %q", lines, want)
	}
}

func TestStartWritesTheEmbeddedSeatConfigIntoTheSeatDirectoryBeforeStartingAUnit(t *testing.T) {
	fake := fakeDeps(fakeOptions{})
	assertDeepEqual(t, Run(context.Background(), []string{"start", "--name", "seat", "--", "omp"}, fake.deps), 0)
	if len(fake.writes) != 1 {
		t.Fatalf("writes = %d, want 1 seat config write", len(fake.writes))
	}
	write := fake.writes[0]
	assertDeepEqual(t, write.path, testSeatConf)
	assertDeepEqual(t, write.perm, fs.FileMode(0o600))
	assertDeepEqual(t, write.data, sourceSeatConf(t))
}

func TestStartPassesTheWrittenSeatConfigToTmuxWithoutAShareDirectory(t *testing.T) {
	fake := fakeDeps(fakeOptions{})
	assertDeepEqual(t, Run(context.Background(), []string{"start", "--name", "seat", "--", "omp"}, fake.deps), 0)
	if len(fake.writes) != 1 {
		t.Fatalf("writes = %d, want 1 seat config write", len(fake.writes))
	}
	assertDeepEqual(t, fake.calls[1], recordedCall{
		command: systemdRunCommand,
		args: []string{
			"--user", "--unit=test-seat-seat", "--slice=test-agents.slice",
			"-p", "Type=exec", "--collect", "--",
			testTmux, "-S", testSocket, "-f", fake.writes[0].path, "-D",
		},
	})
}

func TestStartReportsAConfigWriteFailureBeforeStartingAUnit(t *testing.T) {
	fake := fakeDeps(fakeOptions{writeErr: errors.New("no space left on device")})
	assertDeepEqual(t, Run(context.Background(), []string{"start", "--name", "seat", "--", "omp"}, fake.deps), 1)
	assertDeepEqual(t, recordedCommands(fake.calls), []string{systemctlCommand})
	assertContainsLine(t, fake.errors, "write seat config: no space left on device")
}

func TestStartStopsTheUnitWhenTheWorkingDirectoryCannotBeRead(t *testing.T) {
	fake := fakeDeps(fakeOptions{cwdErr: errors.New("working directory was removed")})
	assertDeepEqual(t, Run(context.Background(), []string{"start", "--name", "seat", "--", "omp"}, fake.deps), 1)
	assertContainsLine(t, fake.errors, "working directory was removed")
	assertDeepEqual(t, fake.calls[len(fake.calls)-1], recordedCall{
		command: systemctlCommand,
		args:    []string{"--user", "stop", "test-seat-seat"},
	})
}

func TestStartAbortsSocketPollWhenTheContextIsCancelled(t *testing.T) {
	fake := fakeDeps(fakeOptions{sleepErr: context.Canceled})
	fake.deps.Files.Connect = func(context.Context, string) bool { return false }
	assertDeepEqual(t, Run(context.Background(), []string{"start", "--name", "seat", "--", "omp"}, fake.deps), 1)
	assertContainsLine(t, fake.errors, context.Canceled.Error())
}
