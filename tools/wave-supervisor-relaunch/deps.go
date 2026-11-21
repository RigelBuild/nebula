package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec" //nolint:depguard // This CLI owns bounded cotal, systemctl, zellij and spawn-agent subprocesses.
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/cotalreach"
	"github.com/RigelBuild/nebula/tools/go-lib/relaunch"
	"github.com/RigelBuild/nebula/tools/go-lib/waveseats"
	"github.com/RigelBuild/nebula/tools/wave-supervisor-relaunch/seats"
)

func defaultDeps(cfg Config, getenv envGetter) (runDeps, error) {
	paths := toolPaths{State: filepath.Join(cfg.StateDir, "wave-supervisor-relaunch.json"), Hold: filepath.Join(cfg.StateDir, "wave-relaunch-hold.json"), Manifest: filepath.Join(cfg.StateDir, "wave-relaunch-manifest.json"), RestartRequest: filepath.Join(cfg.StateDir, "wave-zellij-restart-request.json"), Sessions: cfg.SessionsDir, Home: cfg.HomeDir, Proc: "/proc", Seats: cfg.SeatsPath, Cgroup: "/sys/fs/cgroup"}
	fsys := relaunch.NodeFileSystem{}
	runner := processRunner(runProcess)
	cotal := func(ctx context.Context, argv []string) (cotalreach.CommandResult, error) {
		return runner(ctx, append([]string{cotalCommand}, argv...), "")
	}
	readCotal := func(ctx context.Context, argv []string) (string, error) {
		result, err := cotal(ctx, argv)
		if err != nil {
			return "", err
		}
		return result.Stdout, nil
	}
	identity := func(ctx context.Context) string {
		runtimeDir := getenv("XDG_RUNTIME_DIR")
		if runtimeDir == "" {
			runtimeDir = "/run/user/" + strconv.Itoa(os.Getuid())
		}
		return zellijServerIdentity(ctx, "/proc", runtimeDir, cfg.ZellijUnit, fsys, runner)
	}
	return buildDefaultDeps(cfg, getenv, paths, fsys, runner, cotal, readCotal, identity), nil
}

func buildDefaultDeps(cfg Config, getenv envGetter, paths toolPaths, fsys relaunch.NodeFileSystem, runner processRunner, cotal func(context.Context, []string) (cotalreach.CommandResult, error), readCotal func(context.Context, []string) (string, error), identity func(context.Context) string) runDeps {
	hosts := make(waveseats.HostSet, len(cfg.Hosts))
	for _, host := range cfg.Hosts {
		hosts[waveseats.SeatHost(host)] = struct{}{}
	}
	deps := runDeps{Config: cfg, FS: fsys, Paths: paths, Now: time.Now, Sleep: sleepContext,
		ServiceResult: getenv("SERVICE_RESULT"), ServiceExitStatus: getenv("SERVICE_EXIT_STATUS"),
		Status: func(ctx context.Context) (any, error) {
			text, err := readCotal(ctx, []string{"invoke", "manager", "status", spaceFlag, cfg.CotalSpace, "--json"})
			if err != nil {
				return nil, err
			}
			var envelope struct {
				Result struct {
					Data json.RawMessage `json:"data"`
				} `json:"result"`
			}
			if err := json.Unmarshal([]byte(text), &envelope); err != nil {
				return nil, err
			}
			var value any
			if err := json.Unmarshal(envelope.Result.Data, &value); err != nil {
				return nil, err
			}
			return value, nil
		},
		PS: func(ctx context.Context) (string, error) { return readCotal(ctx, cotalreach.BuildPsArgs()) },
		Endpoints: func(ctx context.Context) (string, error) {
			return readCotal(ctx, []string{"endpoints", spaceFlag, cfg.CotalSpace})
		},
		Trigger: func(ctx context.Context) (Trigger, error) {
			return readSystemTrigger(ctx, runner, fsys, cfg.CotalManagerUnit, cfg.ZellijUnit, identity)
		},
		StopSlice: func(ctx context.Context) (bool, error) {
			result, err := runner(ctx, []string{systemctlCommand, userFlag, "stop", cfg.WaveSlice}, "")
			return err == nil && result.ExitCode == 0, err
		},
		StopSeats: func(ctx context.Context, names []string) error {
			return stopManagedSeats(ctx, names, cfg.CotalSpace, runner)
		},
		ProcScopesPresent: func() bool { return waveProcessesPresent("/proc", fsys, cfg.WaveSlice) },
		SeatNames: func(ctx context.Context) (map[string]struct{}, error) {
			return readActiveSeatNames(ctx, paths.Seats, hosts)
		},
		ReadRootCWD:    func(ctx context.Context) (string, error) { return readActiveRootCWD(ctx, paths.Seats, hosts) },
		ZellijSessions: func(ctx context.Context) (string, error) { return liveZellijSession(ctx, runner, cfg.ZellijSession) },
		ProbeZellij:    func(ctx context.Context) (bool, error) { return probeZellij(ctx, runner, cfg.ZellijSession) },
		ZellijIdentity: identity,
		ZellijUnitProcs: func(ctx context.Context) (string, error) {
			return zellijUnitProcesses(ctx, runner, fsys, cfg.ZellijUnit)
		},
		RestartZellij: func(ctx context.Context) (bool, error) {
			result, err := runner(ctx, []string{systemctlCommand, userFlag, "restart", "--no-block", cfg.ZellijUnit}, "")
			return err == nil && result.ExitCode == 0, err
		},
		StartRelaunch: func(ctx context.Context) (bool, error) {
			result, err := runner(ctx, []string{systemctlCommand, userFlag, "start", "--no-block", cfg.RelaunchUnit}, "")
			return err == nil && result.ExitCode == 0, err
		},
		Spawn: func(ctx context.Context, args []string, cwd string) (int, error) {
			result, err := runner(ctx, append([]string{"spawn-agent"}, args...), cwd)
			return result.ExitCode, err
		},
		DM: func(ctx context.Context, name, text string) (bool, error) {
			result, err := cotal(ctx, cotalreach.BuildDmArgs(name, text))
			return err == nil && result.ExitCode == 0, err
		},
		Holders: func(ctx context.Context) (string, error) {
			return readCommandStdout(ctx, runner, []string{"wave-seat-holder", "list", "--json"})
		},
		Runner:        runner,
		SeatThreshold: getenv(seats.ThresholdEnv),
		WriteLog:      func(line string) error { _, err := fmt.Fprintln(os.Stdout, line); return err },
	}
	deps.EndpointsClear = func(ctx context.Context) (bool, error) { return endpointsClear(ctx, cfg, readCotal, deps.SeatNames) }
	return deps
}

func endpointsClear(ctx context.Context, cfg Config, readCotal func(context.Context, []string) (string, error), seatNames func(context.Context) (map[string]struct{}, error)) (bool, error) {
	text, err := readCotal(ctx, []string{"endpoints", spaceFlag, cfg.CotalSpace})
	if err != nil {
		return false, err
	}
	names, err := seatNames(ctx)
	if err != nil {
		return false, err
	}
	return len(ParseEligibleEndpointNames(text, cfg.RootSeat, cfg.EndpointKind, names)) == 0, nil
}

// liveZellijSession returns session when list-sessions shows it live, and "" otherwise. A failed
// or non-zero list-sessions run reads as not live, the same as before.
func liveZellijSession(ctx context.Context, runner processRunner, session string) (string, error) {
	result, err := runner(ctx, []string{zellijCommand, "list-sessions", "--no-formatting"}, "")
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 || !relaunch.SessionLive(result.Stdout, session) {
		return "", nil
	}
	return session, nil
}

func probeZellij(ctx context.Context, runner processRunner, session string) (bool, error) {
	tabs, err := probeCall(ctx, runner, []string{zellijCommand, "--session", session, "action", "list-tabs", "--json"})
	if err != nil {
		return false, err
	}
	panes, err := probeCall(ctx, runner, []string{zellijCommand, "--session", session, "action", "list-panes", "--json", "--all"})
	if err != nil {
		return false, err
	}
	return probeGoodOutput(tabs, panes), nil
}

func zellijUnitProcesses(ctx context.Context, runner processRunner, fsys relaunch.FileSystem, unit string) (string, error) {
	show, err := runner(ctx, []string{systemctlCommand, userFlag, "show", unit, "--property=ControlGroup", "--value"}, "")
	if err != nil || show.ExitCode != 0 {
		return "", err
	}
	group := strings.TrimSpace(show.Stdout)
	if !strings.HasPrefix(group, "/") {
		return "", nil
	}
	data, err := fsys.ReadFile(filepath.Join("/sys/fs/cgroup", group, "cgroup.procs"))
	return string(data), err
}

func readActiveSeatNames(ctx context.Context, seatsPath string, hosts waveseats.HostSet) (map[string]struct{}, error) {
	list, err := waveseats.LoadSeatList(ctx, seatsPath, waveseats.NodeStoreFileSystem{}, hosts)
	if err != nil {
		return nil, err
	}
	names := map[string]struct{}{}
	for _, seat := range list.Seats {
		if seat.State == waveseats.SeatV2Active {
			names[seat.Name] = struct{}{}
		}
	}
	return names, nil
}

// readActiveRootCWD returns the active root seat's recorded cwd, or "" when no root seat is active.
func readActiveRootCWD(ctx context.Context, seatsPath string, hosts waveseats.HostSet) (string, error) {
	list, err := waveseats.LoadSeatList(ctx, seatsPath, waveseats.NodeStoreFileSystem{}, hosts)
	if err != nil {
		return "", err
	}
	root := waveseats.ActiveRoot(list.Seats)
	for _, seat := range list.Seats {
		if seat.Name == root {
			return seat.CWD, nil
		}
	}
	return "", nil
}

func runProcess(ctx context.Context, argv []string, cwd string) (cotalreach.CommandResult, error) {
	if len(argv) == 0 {
		return cotalreach.CommandResult{}, errors.New("empty command")
	}
	started := time.Now()
	result, err := execRunner(ctx, argv, cwd)
	if err != nil {
		return cotalreach.CommandResult{}, err
	}
	if argv[0] == cotalCommand && result.ExitCode != 0 {
		return cotalreach.CommandResult{}, errors.New(cotalFailure(argv[1:], result, time.Since(started)))
	}
	return result, nil
}

func readSystemTrigger(ctx context.Context, runner processRunner, fsys relaunch.FileSystem, managerUnit, zellijUnit string, identity func(context.Context) string) (Trigger, error) {
	show, err := runner(ctx, []string{systemctlCommand, userFlag, "show", managerUnit, zellijUnit, "--property=Id,ActiveEnterTimestamp,InvocationID,NRestarts,ActiveState,Result"}, "")
	if err != nil {
		return Trigger{}, err
	}
	if show.ExitCode != 0 {
		return Trigger{}, errors.New("systemctl show failed")
	}
	boot, err := fsys.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return Trigger{}, fmt.Errorf("read boot id: %w", err)
	}
	up, err := fsys.ReadFile("/proc/uptime")
	if err != nil {
		return Trigger{}, fmt.Errorf("read uptime: %w", err)
	}
	return Trigger{BootID: strings.TrimSpace(string(boot)), UptimeS: parseUptime(string(up)), Units: parseUnitFacts(show.Stdout), ZellijServer: identity(ctx), ManagerUnit: managerUnit}, nil
}

func execRunner(ctx context.Context, args []string, cwd string) (cotalreach.CommandResult, error) {
	if len(args) == 0 {
		return cotalreach.CommandResult{}, errors.New("empty command")
	}
	// #nosec G204 -- Callers supply fixed executables and argv, never a shell; cancellation kills only this owned child.
	command := exec.CommandContext(ctx, args[0], args[1:]...)
	command.Dir = cwd
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return cotalreach.CommandResult{ExitCode: exitErr.ExitCode(), Stdout: stdout.String(), Stderr: stderr.String()}, nil
		}
		return cotalreach.CommandResult{}, fmt.Errorf("run %s: %w", args[0], err)
	}
	return cotalreach.CommandResult{Stdout: stdout.String(), Stderr: stderr.String()}, nil
}

// seatStepDeps wires the seat step to the relauncher's runner, readers and log. A failed log write
// goes to stderr, then to slog if stderr fails too; it never changes run's exit code.
func seatStepDeps(ctx context.Context, deps runDeps, stderr io.Writer) seats.Deps {
	return seats.Deps{
		FS:          deps.FS,
		CgroupRoot:  deps.Paths.Cgroup,
		Threshold:   deps.SeatThreshold,
		ReadTimeout: seats.ReadTimeout,
		Hold:        func() bool { return holdActive(deps) },
		SeatNames:   deps.SeatNames,
		PS:          deps.PS,
		Status:      deps.Status,
		Holders:     deps.Holders,
		Processes:   func() ([]seats.Process, error) { return seatProcesses(deps) },
		Stop: func(ctx context.Context, name string) error {
			return stopOneSeat(ctx, name, deps.Config.CotalSpace, deps.Runner)
		},
		Restart: func(ctx context.Context, name string) (int, error) {
			restartCtx, cancel := context.WithTimeout(ctx, seats.ReadTimeout)
			defer cancel()
			result, err := deps.Runner(restartCtx, []string{deps.Config.SeatCLI, "restart", name}, "")
			return result.ExitCode, err
		},
		Now:   deps.now,
		Sleep: deps.sleep,
		Log: func(line string) {
			if err := deps.log(ctx, line); err != nil {
				if _, writeErr := fmt.Fprintln(stderr, "wave-supervisor-relaunch: seat step log failed:", err); writeErr != nil {
					slog.Warn("seat step log failed", "error", err, "stderr_error", writeErr)
				}
			}
		},
	}
}

// seatProcesses walks the wave slice's omp processes once and reads each one's seat name and cgroup.
func seatProcesses(deps runDeps) ([]seats.Process, error) {
	pids, err := listSliceOmpPIDs(deps.Paths.Proc, deps.FS, deps.Config.WaveSlice)
	if err != nil {
		return nil, err
	}
	procs := make([]seats.Process, 0, len(pids))
	for _, pid := range pids {
		environ, ok := readText(deps.FS, filepath.Join(deps.Paths.Proc, pid, "environ"))
		if !ok {
			continue
		}
		name := processAgentName(string(environ))
		if name == "" {
			continue
		}
		cgroup, _ := readText(deps.FS, filepath.Join(deps.Paths.Proc, pid, "cgroup"))
		procs = append(procs, seats.Process{Name: name, Cgroup: string(cgroup)})
	}
	return procs, nil
}

// readCommandStdout runs one read through the runner and returns stdout. A non-zero exit names
// the real binary, so a holder failure is not reported as a cotal failure.
func readCommandStdout(ctx context.Context, runner processRunner, argv []string) (string, error) {
	started := time.Now()
	result, err := runner(ctx, argv, "")
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		stderr, _, _ := strings.Cut(strings.TrimSpace(result.Stderr), "\n")
		return "", fmt.Errorf("%s %s exit=%d ms=%d stderr=%s", argv[0], argv[1], result.ExitCode, time.Since(started).Milliseconds(), stderr)
	}
	return result.Stdout, nil
}
