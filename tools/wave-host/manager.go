package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec" //nolint:depguard // The login manager runs the operator-supplied argv.
	"regexp"
	"syscall"
	"time"
)

// stopGrace bounds the wait after SIGTERM before SIGKILL. It matches the 30s
// kill grace in tools/root-install-retry; packages cannot share the constant.
const stopGrace = 30 * time.Second

// killWait bounds the wait after SIGKILL before the manager is reported stuck.
const killWait = 5 * time.Second

// errStillRunning reports that the recorded manager outlived its bound.
var errStillRunning = errors.New("host manager still running after bound")

const managerPollInterval = 50 * time.Millisecond

// managerHost runs the login-start host manager. A stop recorded for one login
// blocks start until that login ends.
type managerHost struct {
	statePath string
	host      string
	login     func(context.Context) (string, error)
	identify  func(int) (Identity, error)
	launch    func(context.Context, []string) (Identity, func() error, error)
	signal    func(int, syscall.Signal) error
	// Bounds are fields so tests can use milliseconds; production sets them from the constants.
	stopGrace time.Duration
	killWait  time.Duration
	poll      time.Duration
}

func (m managerHost) hostName() (string, error) {
	if m.host != "" {
		return m.host, nil
	}
	return os.Hostname()
}

// alive reports whether the recorded process is still the same process.
func (m managerHost) alive(pid int, token string) (bool, error) {
	if pid <= 0 || token == "" {
		return false, nil
	}
	current, err := m.identify(pid)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("identify host manager: %w", err)
	}
	if isMalformedDottedToken(token) {
		return false, fmt.Errorf("recorded manager token unreadable")
	}
	return sameStartToken(token, current.StartToken), nil
}

// start runs the manager argv and blocks until it exits. A clean exit that
// stop requested is success; any other exit is recorded as blocked.
func (m managerHost) start(ctx context.Context, argv []string) error {
	if len(argv) == 0 {
		return errors.New("missing manager command")
	}
	lock, err := lockFile(ctx, m.statePath+".lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	current, err := m.login(ctx)
	if err != nil {
		return fmt.Errorf("read login session: %w", err)
	}
	state, err := Load(m.statePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		running, aliveErr := m.alive(state.ManagerPID, state.ManagerToken)
		if aliveErr != nil {
			return aliveErr
		}
		if running {
			return errors.New("host manager is already running")
		}
		if state.State == "stopping" && state.StopLoginSession == current {
			return errors.New("host manager is stopping this login")
		}
		if state.State == "stopped" && state.StopLoginSession == current {
			return errors.New("host manager was stopped this login")
		}
	}
	host, err := m.hostName()
	if err != nil {
		return err
	}
	if err := Save(m.statePath, State{Version: 1, State: "starting", Host: host}); err != nil {
		return err
	}
	id, wait, err := m.launch(ctx, argv)
	if err != nil {
		return errors.Join(err, Save(m.statePath, State{Version: 1, State: "blocked", Host: host, Reason: err.Error()}))
	}
	if err := Save(m.statePath, State{Version: 1, State: "ready", Host: host, ManagerPID: id.PID, ManagerToken: id.StartToken}); err != nil {
		// The child runs but is unrecorded. Release the lock, then drain with bounds.
		reason := fmt.Sprintf("record ready state: %v", err)
		return errors.Join(err, m.abandon(ctx, host, id, reason, lock, wait))
	}
	// Release before the blocking wait: stop needs the lock to record stopping and signal.
	if err := lock.Close(); err != nil {
		return errors.Join(err, m.abandon(ctx, host, id, "close state lock", nil, wait))
	}
	waitErr := wait()
	reason := "host manager exited"
	if waitErr != nil {
		reason = waitErr.Error()
	}
	stopped, err := m.recordExit(ctx, host, id, reason)
	if err != nil || stopped {
		return err
	}
	return errors.New(reason)
}

// recordExit records blocked under the lock unless a stop landed meanwhile; stopped reports that.
func (m managerHost) recordExit(ctx context.Context, host string, id Identity, reason string) (bool, error) {
	lock, err := lockFile(ctx, m.statePath+".lock")
	if err != nil {
		return false, errors.Join(errors.New(reason), err)
	}
	now, err := Load(m.statePath)
	if err == nil && (now.State == "stopping" || now.State == "stopped") {
		return true, lock.Close()
	}
	saveErr := Save(m.statePath, State{Version: 1, State: "blocked", Host: host, Reason: reason, ManagerPID: id.PID, ManagerToken: id.StartToken})
	return false, errors.Join(saveErr, lock.Close())
}

// abandon drains a child that could not be recorded, then records blocked with
// its identity. A nil lock means the caller already released it.
func (m managerHost) abandon(ctx context.Context, host string, id Identity, reason string, lock *os.File, wait func() error) error {
	if lock != nil {
		if err := lock.Close(); err != nil {
			return errors.Join(err, wait())
		}
	}
	drainErr := m.drain(ctx, id.PID, id.StartToken)
	waitErr := wait()
	fail := errors.Join(drainErr, waitErr)
	blockedErr := m.recordBlocked(ctx, host, State{ManagerPID: id.PID, ManagerToken: id.StartToken}, errors.New(reason))
	return errors.Join(fail, blockedErr)
}

// stop records stopping, signals the recorded manager, waits for it to exit,
// then records a sticky stop for the current login.
func (m managerHost) stop(ctx context.Context) error {
	lock, err := lockFile(ctx, m.statePath+".lock")
	if err != nil {
		return err
	}
	current, err := m.login(ctx)
	if err != nil {
		return errors.Join(fmt.Errorf("read login session: %w", err), lock.Close())
	}
	host, err := m.hostName()
	if err != nil {
		return errors.Join(err, lock.Close())
	}
	state, err := Load(m.statePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.Join(err, lock.Close())
	}
	if err == nil && state.State == "stopped" && state.StopLoginSession == current {
		return lock.Close()
	}
	if err := Save(m.statePath, State{Version: 1, State: "stopping", Host: host, StopLoginSession: current, ManagerPID: state.ManagerPID, ManagerToken: state.ManagerToken}); err != nil {
		return errors.Join(err, lock.Close())
	}
	if err := lock.Close(); err != nil {
		return err
	}
	if err := m.drain(ctx, state.ManagerPID, state.ManagerToken); err != nil {
		return errors.Join(err, m.recordBlocked(ctx, host, state, err))
	}
	lock, err = lockFile(ctx, m.statePath+".lock")
	if err != nil {
		return err
	}
	now, err := Load(m.statePath)
	if err != nil {
		return errors.Join(fmt.Errorf("reload state before stopped: %w", err), lock.Close())
	}
	if now.State != "stopping" {
		return errors.Join(fmt.Errorf("state changed to %q while stopping; not overwriting", now.State), lock.Close())
	}
	return errors.Join(Save(m.statePath, State{Version: 1, State: "stopped", Host: host, StopLoginSession: current}), lock.Close())
}

// recordBlocked keeps the recorded manager identity so a later start still sees it.
func (m managerHost) recordBlocked(ctx context.Context, host string, state State, cause error) error {
	lock, err := lockFile(ctx, m.statePath+".lock")
	if err != nil {
		return err
	}
	return errors.Join(Save(m.statePath, State{Version: 1, State: "blocked", Host: host, Reason: cause.Error(), ManagerPID: state.ManagerPID, ManagerToken: state.ManagerToken}), lock.Close())
}

// drain sends SIGTERM, then SIGKILL after stopGrace; alive gates every signal against PID reuse.
func (m managerHost) drain(ctx context.Context, pid int, token string) error {
	running, err := m.alive(pid, token)
	if err != nil || !running {
		return err
	}
	if err := m.signal(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal host manager: %w", err)
	}
	if err := m.waitGone(ctx, pid, token, m.stopGrace); err == nil {
		return nil
	} else if !errors.Is(err, errStillRunning) {
		return err
	}
	running, err = m.alive(pid, token)
	if err != nil || !running {
		return err
	}
	if err := m.signal(pid, syscall.SIGKILL); err != nil {
		return fmt.Errorf("kill host manager after %s: %w", m.stopGrace, err)
	}
	if err := m.waitGone(ctx, pid, token, m.killWait); err != nil {
		return fmt.Errorf("host manager survived SIGKILL: %w", err)
	}
	return nil
}

// waitGone polls until the identity is gone, errStillRunning after the bound,
// or the caller's context ends.
func (m managerHost) waitGone(ctx context.Context, pid int, token string, bound time.Duration) error {
	deadline := time.NewTimer(bound)
	defer deadline.Stop()
	ticker := time.NewTicker(m.poll)
	defer ticker.Stop()
	for {
		running, err := m.alive(pid, token)
		if err != nil || !running {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for host manager to exit: %w", ctx.Err())
		case <-deadline.C:
			return errStillRunning
		case <-ticker.C:
		}
	}
}

// launchManager starts argv as a child, records its identity, and returns a
// wait function that reaps it. Identity failure kills the child.
func launchManager(_ context.Context, argv []string) (Identity, func() error, error) {
	//nolint:gosec // The LaunchAgent supplies the manager argv from operator configuration.
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return Identity{}, nil, fmt.Errorf("start host manager: %w", err)
	}
	id, err := Identify(cmd.Process.Pid)
	if err != nil {
		return Identity{}, nil, errors.Join(fmt.Errorf("identify host manager: %w", err), cmd.Process.Kill(), cmd.Wait())
	}
	return id, cmd.Wait, nil
}

// loginCreator matches the domain header's loginwindow; a new login has a new loginwindow.
var loginCreator = regexp.MustCompile(`(?m)^\tcreator = loginwindow\[([0-9]+)\]$`)

// loginToken names the current login by the identity of its GUI launchd domain.
func loginToken(runner commandRunner, domain string, identify func(int) (Identity, error)) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		output, err := runner(ctx, "launchctl", "print", domain)
		if err != nil {
			return "", fmt.Errorf("launchctl print %s: %w", domain, err)
		}
		match := loginCreator.FindSubmatch(output)
		if len(match) != 2 {
			return "", fmt.Errorf("launchctl print %s has no loginwindow creator", domain)
		}
		var pid int
		if _, err := fmt.Sscanf(string(match[1]), "%d", &pid); err != nil || pid <= 0 {
			return "", errors.New("launchctl print has invalid pid")
		}
		id, err := identify(pid)
		if err != nil {
			return "", fmt.Errorf("identify login domain: %w", err)
		}
		if id.StartToken == "" {
			return "", errors.New("empty login token")
		}
		return fmt.Sprintf("%d:%s", id.PID, id.StartToken), nil
	}
}

// newManagerHost wires the production seams for the state file at path.
func newManagerHost(path string, uid int) managerHost {
	return managerHost{
		statePath: path,
		login:     loginToken(runCommand, fmt.Sprintf("gui/%d", uid), Identify),
		identify:  Identify,
		launch:    launchManager,
		signal:    syscall.Kill,
		stopGrace: stopGrace,
		killWait:  killWait,
		poll:      managerPollInterval,
	}
}
