package seat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/RigelBuild/nebula/tools/wave-seat-holder/internal/pane"
	"github.com/RigelBuild/nebula/tools/wave-seat-holder/internal/socket"
)

const (
	tmuxCommand        = "tmux"
	sessionName        = pane.SessionName
	waveHostCommand    = "wave-host"
	systemctlCommand   = "systemctl"
	systemctlUserFlag  = "--user"
	systemdRunCommand  = "systemd-run"
	defaultUnitPrefix  = "wave-seat-"
	defaultSlice       = "wave-agents.slice"
	startupTimeout     = 10 * time.Second
	pollInterval       = 100 * time.Millisecond
	socketPollInterval = 50 * time.Millisecond
)

var (
	hostPIDsPattern = regexp.MustCompile(`^\d+ \d+\s*$`)
	pidPattern      = regexp.MustCompile(`^\d+$`)
)

// seatLaunch holds the names and paths of one Linux start attempt.
type seatLaunch struct {
	directory  string
	socketPath string
	unit       string
	tmux       string
	config     string
	command    []string
}

// startSeat starts a seat and prints its holder and agent process IDs.
func startSeat(ctx context.Context, name string, command []string, deps Deps) int {
	if deps.Platform == "darwin" {
		return startDarwinSeat(ctx, name, command, deps)
	}
	return startLinuxSeat(ctx, name, command, deps)
}

// startDarwinSeat delegates the start to wave-host, which owns the seat on macOS.
func startDarwinSeat(ctx context.Context, name string, command []string, deps Deps) int {
	home, _ := deps.Getenv("HOME") // An unset HOME and an empty one both fail below.
	if home == "" {
		return reportFailure(deps, "HOME must be set")
	}
	socketPath, err := socket.Path(name, filepath.Join(home, "agents/run/seats"), "darwin")
	if err != nil {
		return reportFailure(deps, err.Error())
	}
	config, err := writeSeatConfig(deps, filepath.Join(home, "agents/run/seats"), name)
	if err != nil {
		return reportFailure(deps, err.Error())
	}
	cwd, err := deps.Cwd()
	if err != nil {
		return reportFailure(deps, err.Error())
	}
	args := make([]string, 0, 10+len(command))
	args = append(args, "seat-start", "--name", name, "--socket", socketPath, "--conf", config, "--cwd", cwd, "--")
	args = append(args, command...)
	result, err := deps.Spawn(ctx, waveHostCommand, args, false)
	if err != nil {
		return reportFailure(deps, err.Error())
	}
	if result.Code != 0 {
		detail := strings.TrimSpace(result.Stderr)
		if detail == "" {
			detail = fmt.Sprintf("wave-host exited %d", result.Code)
		}
		return reportFailure(deps, detail)
	}
	if !hostPIDsPattern.MatchString(result.Stdout) {
		return reportFailure(deps, "wave-host returned invalid process ids")
	}
	deps.Stdout(strings.TrimRightFunc(result.Stdout, unicode.IsSpace) + "\n")
	return 0
}

// startLinuxSeat runs the seat as a transient systemd user unit around tmux.
func startLinuxSeat(ctx context.Context, name string, command []string, deps Deps) int {
	directory, err := socket.Directory(deps.Getenv)
	if err != nil {
		return reportFailure(deps, err.Error())
	}
	socketPath, err := socket.Path(name, directory, deps.Platform)
	if err != nil {
		return reportFailure(deps, err.Error())
	}
	unit := envOr(deps.Getenv, "WAVE_SEAT_UNIT_PREFIX", defaultUnitPrefix) + name
	if deps.Files.Connect(ctx, socketPath) {
		return seatAlreadyLive(name, deps)
	}
	loaded, err := unitLoaded(ctx, unit, deps)
	if err != nil {
		return reportFailure(deps, err.Error())
	}
	if loaded {
		return seatAlreadyLive(name, deps)
	}
	// The user manager's PATH has no tmux on NixOS; resolve it from ours.
	tmux, ok := deps.Which(tmuxCommand)
	if !ok {
		return reportFailure(deps, "tmux is not on PATH")
	}
	launch := seatLaunch{directory: directory, socketPath: socketPath, unit: unit, tmux: tmux, command: command}
	config, err := writeSeatConfig(deps, directory, name)
	if err != nil {
		return reportFailure(deps, err.Error())
	}
	launch.config = config
	if err := startSeatUnit(ctx, launch, envOr(deps.Getenv, "WAVE_SEAT_SLICE", defaultSlice), deps); err != nil {
		return reportFailure(deps, err.Error())
	}
	// The unit is running, so every failure from here must stop it.
	if err := finishSeat(ctx, launch, deps); err != nil {
		stopSeatUnit(ctx, deps, unit)
		return reportFailure(deps, err.Error())
	}
	return 0
}

// seatAlreadyLive reports a seat that another holder already serves.
func seatAlreadyLive(name string, deps Deps) int {
	deps.Stderr(fmt.Sprintf("wave-seat-holder: seat %s is already live\n", name))
	return 4
}

// writeSeatConfig creates the seat's directory and writes the embedded tmux configuration to <name>.conf inside it,
// at mode 0600. The file sits beside the seat's socket, so no other seat shares it. It returns the file's path.
//
// Every step after the mkdir is relative to one open directory handle, so a path swapped for a link after the
// check cannot redirect the write. The config is recreated, not reopened, so a planted hardlink is never written.
func writeSeatConfig(deps Deps, directory, name string) (string, error) {
	if err := deps.Files.Mkdir(directory, 0o700); err != nil {
		return "", err
	}
	dir, err := deps.Files.OpenSeatDir(directory)
	if err != nil {
		return "", err
	}
	defer dir.Close()
	if err := dir.WriteFile(name+".conf", seatConfig); err != nil {
		return "", fmt.Errorf("write seat config: %w", err)
	}
	return filepath.Join(directory, name+".conf"), nil
}

// envOr returns the variable's value when it is set, even when empty. It falls back only when unset.
func envOr(getenv socket.Environment, key, fallback string) string {
	if value, ok := getenv(key); ok {
		return value
	}
	return fallback
}

// unitLoaded reports whether systemd already has the seat unit loaded.
func unitLoaded(ctx context.Context, unit string, deps Deps) (bool, error) {
	state, err := deps.Spawn(ctx, systemctlCommand, []string{systemctlUserFlag, "show", "-p", "LoadState", unit}, false)
	if err != nil {
		return false, err
	}
	return state.Code == 0 && strings.TrimSpace(state.Stdout) == "LoadState=loaded", nil
}

// startSeatUnit runs the tmux server under a transient systemd unit.
func startSeatUnit(ctx context.Context, launch seatLaunch, slice string, deps Deps) error {
	args := []string{
		systemctlUserFlag,
		"--unit=" + launch.unit,
		"--slice=" + slice,
		"-p", "Type=exec",
		"--collect",
		"--",
		launch.tmux, "-S", launch.socketPath, "-f", launch.config, "-D",
	}
	result, err := deps.Spawn(ctx, systemdRunCommand, args, false)
	if err != nil {
		return err
	}
	if result.Code != 0 {
		return fmt.Errorf("systemd-run failed: %s", failureDetail(result))
	}
	return nil
}

// finishSeat opens the seat session and reports its process IDs. Any error means the unit must stop.
func finishSeat(ctx context.Context, launch seatLaunch, deps Deps) error {
	if err := waitForSocket(ctx, launch.socketPath, deps); err != nil {
		return err
	}
	if err := pane.Keep(ctx, paneSpawn(deps), launch.tmux, launch.socketPath); err != nil {
		return err
	}
	cwd, err := deps.Cwd()
	if err != nil {
		return err
	}
	args := make([]string, 0, 14+len(launch.command))
	args = append(args, "-N", "-S", launch.socketPath, "new-session", "-d", "-s", sessionName, "-x", "200", "-y", "50", "-c", cwd, "--")
	args = append(args, launch.command...)
	session, err := deps.Spawn(ctx, launch.tmux, args, false)
	if err != nil {
		return err
	}
	if session.Code != 0 {
		return fmt.Errorf("tmux new-session failed: %s", failureDetail(session))
	}
	return waitForProcessIDs(ctx, launch, deps)
}

// waitForSocket polls until a server accepts a connection on socketPath or the startup timeout passes.
func waitForSocket(ctx context.Context, socketPath string, deps Deps) error {
	deadline := deps.Now().Add(startupTimeout)
	for !deps.Files.Connect(ctx, socketPath) {
		if !deps.Now().Before(deadline) {
			return fmt.Errorf("timed out waiting for socket %s", socketPath)
		}
		if err := deps.Sleep(ctx, socketPollInterval); err != nil {
			return err
		}
	}
	return nil
}

// waitForProcessIDs polls until the agent is forked, then prints the holder and agent PIDs.
func waitForProcessIDs(ctx context.Context, launch seatLaunch, deps Deps) error {
	deadline := deps.Now().Add(startupTimeout)
	for {
		holderPID, err := readHolderPID(ctx, launch.unit, deps)
		if err != nil {
			return err
		}
		agentPID, dead, err := agentProcessID(ctx, launch, deps)
		if err != nil {
			return err
		}
		if dead {
			report, err := pane.DeadReport(ctx, paneSpawn(deps), launch.tmux, launch.socketPath)
			if err != nil {
				return err
			}
			return errors.New(report)
		}
		if agentPID != "" {
			if err := pane.Clear(ctx, paneSpawn(deps), launch.tmux, launch.socketPath); err != nil {
				return err
			}
			deps.Stdout(holderPID + " " + agentPID + "\n")
			return nil
		}
		if !deps.Now().Before(deadline) {
			return errors.New("timed out waiting for the seat process ids")
		}
		if err := deps.Sleep(ctx, pollInterval); err != nil {
			return err
		}
	}
}

// readHolderPID returns the unit's MainPID, the holder process.
func readHolderPID(ctx context.Context, unit string, deps Deps) (string, error) {
	main, err := deps.Spawn(ctx, systemctlCommand, []string{systemctlUserFlag, "show", "-p", "MainPID", "--value", unit}, false)
	if err != nil {
		return "", err
	}
	if main.Code != 0 {
		return "", fmt.Errorf("could not read MainPID: %s", failureDetail(main))
	}
	holderPID, ok := processID(main.Stdout)
	if !ok {
		quoted, quoteErr := socket.QuoteJSON(strings.TrimSpace(main.Stdout))
		if quoteErr != nil {
			return "", fmt.Errorf("quote MainPID: %w", quoteErr)
		}
		return "", fmt.Errorf("systemd reported invalid MainPID %s", quoted)
	}
	return holderPID, nil
}

// agentProcessID returns the agent PID once the pane launcher has forked it. It reports dead when the
// launcher's pane is dead, which means the launcher exited before the agent started. tmux 3.4 keeps
// pane_pid after that exit, so the pane's death is checked before its PID is trusted.
func agentProcessID(ctx context.Context, launch seatLaunch, deps Deps) (agentPID string, dead bool, err error) {
	isDead, err := pane.IsDead(ctx, paneSpawn(deps), launch.tmux, launch.socketPath)
	if err != nil {
		return "", false, err
	}
	if isDead {
		return "", true, nil
	}
	paneInfo, err := deps.Spawn(ctx, launch.tmux, []string{"-N", "-S", launch.socketPath, "display", "-p", "-t", sessionName, "#{pane_pid}"}, false)
	if err != nil {
		return "", false, err
	}
	if paneInfo.Code != 0 {
		return "", false, nil
	}
	panePID, ok := processID(paneInfo.Stdout)
	// A pane with no pid means the launcher exited before the agent started.
	if !ok {
		return "", true, nil
	}
	// The pane runs the launcher; the agent is its child, spawned shortly after.
	children, err := deps.Files.ReadText(fmt.Sprintf("/proc/%s/task/%s/children", panePID, panePID))
	if err != nil {
		children = "" // A missing children file means the launcher has not forked yet.
	}
	fields := strings.Fields(children)
	if len(fields) == 0 {
		return "", false, nil
	}
	agentPID, ok = processID(fields[0])
	if !ok {
		return "", false, nil
	}
	return agentPID, false, nil
}

// processID returns the trimmed value when it is a positive decimal process ID.
func processID(value string) (string, bool) {
	normalized := strings.TrimSpace(value)
	if !pidPattern.MatchString(normalized) || strings.Trim(normalized, "0") == "" {
		return "", false
	}
	return normalized, true
}

// stopSeatUnit is best effort: the failure that triggered it is the one the caller reports.
func stopSeatUnit(ctx context.Context, deps Deps, unit string) {
	result, err := deps.Spawn(ctx, systemctlCommand, []string{systemctlUserFlag, "stop", unit}, false)
	if err != nil {
		slog.Error("stopping seat unit", "unit", unit, "error", err)
		return
	}
	if result.Code != 0 {
		slog.Error("stopping seat unit", "unit", unit, "detail", failureDetail(result))
	}
}

// paneSpawn adapts the holder's Spawn to the pane package. Pane commands never inherit stdio.
func paneSpawn(deps Deps) pane.Spawn {
	return func(ctx context.Context, tmux string, args []string) (pane.Result, error) {
		return deps.Spawn(ctx, tmux, args, false)
	}
}
