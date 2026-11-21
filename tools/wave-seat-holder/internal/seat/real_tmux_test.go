package seat

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// realTmuxBinary is the tmux the teardown test runs: WAVE_SEAT_TEST_TMUX, else tmux on PATH.
// It returns "" when neither is set, and the test then skips.
func realTmuxBinary() string {
	if path := os.Getenv("WAVE_SEAT_TEST_TMUX"); path != "" {
		return path
	}
	path, err := exec.LookPath("tmux")
	if err != nil {
		return ""
	}
	return path
}

// realSystemd fakes the user manager for a real tmux server. systemd-run starts a real
// tmux server on the seat socket, and systemctl stop signals only that server's PID.
type realSystemd struct {
	tmux    string
	cmd     *exec.Cmd
	stopped bool
}

// spawn is the Deps.Spawn for the fake systemd. Other commands run as given.
func (r *realSystemd) spawn(ctx context.Context, command string, args []string, inherit bool) (CommandResult, error) {
	switch command {
	case systemdRunCommand:
		return r.startServer(args)
	case systemctlCommand:
		return r.systemctl(args)
	default:
		return spawnCommand(ctx, command, args, inherit)
	}
}

// startServer launches the tmux server the way systemd-run would, from the -S and -f arguments.
func (r *realSystemd) startServer(args []string) (CommandResult, error) {
	cmd := exec.Command(r.tmux, "-S", argAfter(args, "-S"), "-f", argAfter(args, "-f"), "-D")
	if err := cmd.Start(); err != nil {
		return CommandResult{}, err
	}
	r.cmd = cmd
	return CommandResult{}, nil
}

// systemctl answers MainPID with the server's PID, and stop signals that PID and waits for it.
func (r *realSystemd) systemctl(args []string) (CommandResult, error) {
	if slices.Contains(args, "MainPID") {
		if r.cmd == nil || r.cmd.Process == nil {
			return CommandResult{Stdout: "0\n"}, nil
		}
		return CommandResult{Stdout: strconv.Itoa(r.cmd.Process.Pid) + "\n"}, nil
	}
	if !slices.Contains(args, "stop") || r.cmd == nil || r.cmd.Process == nil {
		return CommandResult{}, nil
	}
	if err := r.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return CommandResult{}, err
	}
	// A server stopped by SIGTERM exits with an ExitError; only a failure to wait is a stop failure.
	var exitErr *exec.ExitError
	if err := r.cmd.Wait(); err != nil && !errors.As(err, &exitErr) {
		return CommandResult{}, err
	}
	r.stopped = true
	return CommandResult{}, nil
}

// TestRealTmuxDeadLauncherStopsTheRealServerOnTheSeatSocket runs a real tmux server through the
// fake systemd. A launcher that exits 3 must be reported, and the stop must leave no server on the socket.
func TestRealTmuxDeadLauncherStopsTheRealServerOnTheSeatSocket(t *testing.T) {
	tmux := realTmuxBinary()
	if tmux == "" {
		t.Skip("tmux is not on PATH and WAVE_SEAT_TEST_TMUX is unset")
	}
	// t.TempDir embeds the test name, which pushes the socket path past the 107-byte limit on linux.
	dir, err := os.MkdirTemp("", "wsh")
	if err != nil {
		t.Fatalf("create short socket dir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove socket dir: %v", err)
		}
	})
	socketPath := filepath.Join(dir, "seat.sock")
	systemd := &realSystemd{tmux: tmux}
	t.Cleanup(func() {
		// Only the server this test started; kill it if the stop path never ran.
		if systemd.cmd == nil || systemd.cmd.Process == nil || systemd.stopped {
			return
		}
		if err := systemd.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("killing the test tmux server: %v", err)
		}
		_ = systemd.cmd.Wait() // The kill's exit status is expected; only the kill itself is checked above.
	})
	var errs []string
	deps := Deps{
		Platform: "linux",
		Getenv: lookupMap(map[string]string{
			"WAVE_SEAT_SOCKET_DIR":  dir,
			"WAVE_SEAT_UNIT_PREFIX": "test-seat-",
		}),
		Cwd:   func() (string, error) { return dir, nil },
		Now:   time.Now,
		Sleep: sleepContext,
		Files: Files{
			Mkdir:     os.MkdirAll,
			Lstat:     os.Lstat,
			Chmod:     os.Chmod,
			Getuid:    os.Getuid,
			WriteFile: writePrivateFile,
			Readdir:   readDirNames,
			ReadText:  readFileText,
			Connect:   connectSocket,
		},
		Which:  func(string) (string, bool) { return tmux, true },
		Spawn:  systemd.spawn,
		Stdout: func(string) {},
		Stderr: func(line string) { errs = append(errs, line) },
	}
	if got := Run(context.Background(), []string{"start", "--name", "seat", "--", "sh", "-c", "exit 3"}, deps); got != 1 {
		t.Fatalf("Run = %d, want 1", got)
	}
	if joined := strings.Join(errs, " "); !strings.Contains(joined, "exited with status 3") {
		t.Fatalf("errors = %q, want the launcher's exit status", errs)
	}
	if !systemd.stopped {
		t.Fatal("the unit was not stopped")
	}
	if connectSocket(context.Background(), socketPath) {
		t.Fatal("the seat socket still accepts connections after teardown")
	}
}

// argAfter returns the argument that follows flag, or "" when flag is absent or last.
func argAfter(args []string, flag string) string {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
