package seat

import (
	"context"
	"io/fs"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/wave-seat-holder/internal/socket"
)

const (
	testSocket   = "/run/test-seats/seat.sock"
	testTmux     = "/nix/store/tmux/bin/tmux"
	testSeatConf = "/nix/store/share/wave-seat-holder/seat.conf"
)

// recordedCall is one spawn the holder made, with whether stdio was inherited.
type recordedCall struct {
	command string
	args    []string
	inherit bool
}

// recordedMkdir is one directory the holder created, with the permission it asked for.
type recordedMkdir struct {
	path string
	perm fs.FileMode
}

// fakeOptions selects the behaviour of the fake OS for one test.
type fakeOptions struct {
	liveSocketPaths []string
	socketNames     []string
	loadState       string
	mainPID         string
	panePID         string
	children        []string
	tmuxPath        *string
	newSessionCode  int
	cwdErr          error
	seatConfigErr   error
	sleepErr        error
	// setOptionFails names the remain-on-exit value ("on" or "off") whose set-option fails.
	setOptionFails string
	// paneDeadDisplayErr makes the #{pane_dead} display exit 1 with this stderr.
	paneDeadDisplayErr string
	// deadPane is a launcher that dies before the agent is found.
	deadPane *deadPane
	// diesAfterAgent is a launcher that dies after the agent is found, before remain-on-exit is cleared.
	diesAfterAgent *deadPane
}

// deadPane is a launcher's exit: its status, its signal (empty when it exited), and its last pane output.
type deadPane struct {
	status string
	signal string
	output string
}

// fakeHolder is a Deps with recorded side effects.
type fakeHolder struct {
	deps   Deps
	calls  []recordedCall
	output []string
	errors []string
	mkdirs []recordedMkdir
	// dead is true once the scripted launcher has exited.
	dead bool
	// pane is the scripted launcher exit, if any.
	pane *deadPane
}

// setOptionFailsFor reports whether a set-option call sets the value that options makes fail.
func setOptionFailsFor(options fakeOptions, args []string) bool {
	return options.setOptionFails != "" && args[len(args)-1] == options.setOptionFails
}

// deadOutput is the pane text capture-pane returns: the launcher's output once it has died.
func (f *fakeHolder) deadOutput() string {
	if f.pane == nil {
		return ""
	}
	return f.pane.output
}

// fakeDeps builds Deps whose clock, socket and spawn results are scripted by options.
func fakeDeps(options fakeOptions) *fakeHolder {
	fake := &fakeHolder{pane: options.deadPane, dead: options.deadPane != nil}
	if fake.pane == nil {
		fake.pane = options.diesAfterAgent
	}
	live := make(map[string]bool, len(options.liveSocketPaths))
	for _, path := range options.liveSocketPaths {
		live[path] = true
	}
	started := false
	var now time.Time
	children := append([]string(nil), options.children...)
	if len(children) == 0 {
		children = []string{"789\n"}
	}
	fake.deps = Deps{
		Platform: "linux",
		Getenv: lookupMap(map[string]string{
			"WAVE_SEAT_SOCKET_DIR":  "/run/test-seats",
			"WAVE_SEAT_UNIT_PREFIX": "test-seat-",
			"WAVE_SEAT_SLICE":       "test-agents.slice",
		}),
		Cwd: func() (string, error) {
			if options.cwdErr != nil {
				return "", options.cwdErr
			}
			return "/srv/agent", nil
		},
		Now: func() time.Time { return now },
		Sleep: func(_ context.Context, d time.Duration) error {
			if options.sleepErr != nil {
				return options.sleepErr
			}
			now = now.Add(d)
			return nil
		},
		Files: fakeFiles(fake, options, live, &children, &started),
		Which: fakeWhich(options),
		Spawn: func(_ context.Context, command string, args []string, inherit bool) (CommandResult, error) {
			fake.calls = append(fake.calls, recordedCall{command: command, args: args, inherit: inherit})
			return fake.spawnResult(options, command, args, &started), nil
		},
		SeatConfig: func() (string, error) {
			if options.seatConfigErr != nil {
				return "", options.seatConfigErr
			}
			return testSeatConf, nil
		},
		Stdout: func(line string) { fake.output = append(fake.output, line) },
		Stderr: func(line string) { fake.errors = append(fake.errors, line) },
	}
	return fake
}

// fakeFiles scripts the filesystem: directory creation, socket listing, /proc reads and connects.
func fakeFiles(fake *fakeHolder, options fakeOptions, live map[string]bool, children *[]string, started *bool) Files {
	return Files{
		Mkdir: func(path string, perm fs.FileMode) error {
			fake.mkdirs = append(fake.mkdirs, recordedMkdir{path: path, perm: perm})
			return nil
		},
		Readdir: func(string) ([]string, error) {
			if options.socketNames == nil {
				return nil, fs.ErrNotExist
			}
			return append([]string(nil), options.socketNames...), nil
		},
		ReadText: func(string) (string, error) {
			child := (*children)[0]
			if len(*children) > 1 {
				child = (*children)[0]
				*children = (*children)[1:]
			}
			// The race fixture: the launcher dies once the agent is found.
			if child != "" && options.diesAfterAgent != nil {
				fake.dead = true
			}
			return child, nil
		},
		Connect: func(_ context.Context, path string) bool {
			return live[path] || (*started && path == testSocket)
		},
	}
}

// fakeWhich reports tmux as present unless the options name another path or none.
func fakeWhich(options fakeOptions) func(string) (string, bool) {
	return func(command string) (string, bool) {
		if command != tmuxCommand {
			return "", false
		}
		if options.tmuxPath != nil {
			if *options.tmuxPath == "" {
				return "", false
			}
			return *options.tmuxPath, true
		}
		return testTmux, true
	}
}

// spawnResult scripts the result of one child process for the fake OS.
func (f *fakeHolder) spawnResult(options fakeOptions, command string, args []string, started *bool) CommandResult {
	switch {
	case command == systemdRunCommand:
		*started = true
		return CommandResult{}
	case command == systemctlCommand:
		return systemctlReply(options, args)
	case command == testTmux:
		return f.tmuxReply(options, args)
	default:
		return CommandResult{}
	}
}

// systemctlReply scripts the systemctl queries: the unit's load state and its main PID.
func systemctlReply(options fakeOptions, args []string) CommandResult {
	switch {
	case slices.Contains(args, "LoadState"):
		state := options.loadState
		if state == "" {
			state = "not-found"
		}
		return CommandResult{Stdout: "LoadState=" + state + "\n"}
	case slices.Contains(args, "MainPID"):
		pid := options.mainPID
		if pid == "" {
			pid = "123"
		}
		return CommandResult{Stdout: pid + "\n"}
	default:
		return CommandResult{}
	}
}

// tmuxReply scripts the tmux commands: session creation, remain-on-exit, pane capture and display.
func (f *fakeHolder) tmuxReply(options fakeOptions, args []string) CommandResult {
	switch {
	case slices.Contains(args, "new-session"):
		return CommandResult{Code: options.newSessionCode, Stderr: "failed"}
	case slices.Contains(args, "set-option"):
		if setOptionFailsFor(options, args) {
			return CommandResult{Code: 1, Stderr: "set failed"}
		}
		return CommandResult{}
	case slices.Contains(args, "capture-pane"):
		return CommandResult{Stdout: f.deadOutput()}
	case slices.Contains(args, "display"):
		return f.displayReply(options, args)
	default:
		return CommandResult{}
	}
}

// displayReply scripts the pane queries: pane_dead, pane_dead_status|pane_dead_signal, and pane_pid.
func (f *fakeHolder) displayReply(options fakeOptions, args []string) CommandResult {
	switch {
	case slices.Contains(args, "#{pane_dead}"):
		if options.paneDeadDisplayErr != "" {
			return CommandResult{Code: 1, Stderr: options.paneDeadDisplayErr}
		}
		if f.dead {
			return CommandResult{Stdout: "1\n"}
		}
		return CommandResult{Stdout: "0\n"}
	case slices.Contains(args, "#{pane_dead_status}|#{pane_dead_signal}"):
		if f.dead && f.pane != nil {
			return CommandResult{Stdout: f.pane.status + "|" + f.pane.signal + "\n"}
		}
		return CommandResult{Stdout: "|\n"}
	default:
		if f.dead {
			return CommandResult{Stdout: "\n"}
		}
		pid := options.panePID
		if pid == "" {
			pid = "456"
		}
		return CommandResult{Stdout: pid + "\n"}
	}
}

// lookupMap returns a socket.Environment over values; a missing key reports unset.
func lookupMap(values map[string]string) socket.Environment {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

// assertDeepEqual fails the test when got and want differ.
func assertDeepEqual(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}
