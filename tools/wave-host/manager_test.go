package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fakeManager(t *testing.T, path, login string) managerHost {
	t.Helper()
	return managerHost{
		statePath: path,
		host:      "macbook",
		login:     func(context.Context) (string, error) { return login, nil },
		identify:  func(pid int) (Identity, error) { return Identity{PID: pid, StartToken: "tok"}, nil },
		launch: func(context.Context, []string) (Identity, func() error, error) {
			t.Fatal("launched without expected state")
			return Identity{}, nil, nil
		},
		signal:    func(int, syscall.Signal) error { return nil },
		stopGrace: 20 * time.Millisecond,
		killWait:  20 * time.Millisecond,
		poll:      time.Millisecond,
	}
}

// TestManagerStartRefusedAfterStopThisLogin pins sticky stop: a stop recorded
// for the current login blocks start.
func TestManagerStartRefusedAfterStopThisLogin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Save(path, State{Version: 1, State: "stopped", Host: "macbook", StopLoginSession: "login-1"}); err != nil {
		t.Fatal(err)
	}
	if err := fakeManager(t, path, "login-1").start(context.Background(), []string{"manager"}); err == nil {
		t.Fatal("start succeeded after a sticky stop in the same login")
	}
	got, err := Load(path)
	if err != nil || got.State != "stopped" || got.StopLoginSession != "login-1" {
		t.Fatalf("state changed: %+v, %v", got, err)
	}
}

// TestManagerStartAfterNewLogin pins that a stop from an earlier login does
// not block start in a new login, and that the manager identity is recorded.
func TestManagerStartAfterNewLogin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Save(path, State{Version: 1, State: "stopped", Host: "macbook", StopLoginSession: "login-1"}); err != nil {
		t.Fatal(err)
	}
	m := fakeManager(t, path, "login-2")
	m.launch = func(context.Context, []string) (Identity, func() error, error) {
		return Identity{PID: 42, StartToken: "tok"}, func() error { return nil }, nil
	}
	m.identify = func(int) (Identity, error) { return Identity{}, os.ErrNotExist }
	// The manager exits cleanly with no stop requested, so start reports it blocked.
	err := m.start(context.Background(), []string{"manager"})
	if err == nil {
		t.Fatal("clean unrequested exit must not report success")
	}
	got, loadErr := Load(path)
	if loadErr != nil || got.State != "blocked" || got.ManagerPID != 42 {
		t.Fatalf("state = %+v, %v", got, loadErr)
	}
}

// TestManagerStartUnreadableStateBlocks pins that unreadable state refuses
// start instead of guessing.
func TestManagerStartUnreadableStateBlocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := fakeManager(t, path, "login-1").start(context.Background(), []string{"manager"}); err == nil {
		t.Fatal("start succeeded over unreadable state")
	}
}

// TestManagerStartLoginUnknownBlocks pins that an unknown login token refuses start.
func TestManagerStartLoginUnknownBlocks(t *testing.T) {
	m := fakeManager(t, filepath.Join(t.TempDir(), "state.json"), "")
	m.login = func(context.Context) (string, error) { return "", errors.New("gui domain unreadable") }
	if err := m.start(context.Background(), []string{"manager"}); err == nil {
		t.Fatal("start succeeded without a login token")
	}
}

// TestManagerStartRefusedWhileRunning pins that a live recorded manager blocks a second start.
func TestManagerStartRefusedWhileRunning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Save(path, State{Version: 1, State: "ready", Host: "macbook", ManagerPID: 7, ManagerToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	if err := fakeManager(t, path, "login-1").start(context.Background(), []string{"manager"}); err == nil {
		t.Fatal("second start succeeded while the manager was live")
	}
}

// TestManagerStopRecordsStickyStop pins that stop signals the recorded manager,
// waits for it to exit, and records the stop for this login.
func TestManagerStopRecordsStickyStop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Save(path, State{Version: 1, State: "ready", Host: "macbook", ManagerPID: 7, ManagerToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	gone := false
	m := fakeManager(t, path, "login-1")
	m.identify = func(pid int) (Identity, error) {
		if gone {
			return Identity{}, os.ErrNotExist
		}
		return Identity{PID: pid, StartToken: "tok"}, nil
	}
	m.signal = func(pid int, sig syscall.Signal) error {
		if pid != 7 || sig != syscall.SIGTERM {
			t.Fatalf("signal %d to %d", sig, pid)
		}
		gone = true
		return nil
	}
	if err := m.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil || got.State != "stopped" || got.StopLoginSession != "login-1" {
		t.Fatalf("state = %+v, %v", got, err)
	}
}

// TestManagerStopReusedPIDIsNotSignalled pins PID reuse: a new process with
// the recorded PID but a different start token is never signalled.
func TestManagerStopReusedPIDIsNotSignalled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Save(path, State{Version: 1, State: "ready", Host: "macbook", ManagerPID: 7, ManagerToken: "old"}); err != nil {
		t.Fatal(err)
	}
	m := fakeManager(t, path, "login-1")
	m.identify = func(pid int) (Identity, error) { return Identity{PID: pid, StartToken: "new"}, nil }
	m.signal = func(int, syscall.Signal) error {
		t.Fatal("signalled a reused pid")
		return nil
	}
	if err := m.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestManagerStopIsIdempotentThisLogin pins that a repeated stop in the same login is a no-op.
func TestManagerStopIsIdempotentThisLogin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Save(path, State{Version: 1, State: "stopped", Host: "macbook", StopLoginSession: "login-1"}); err != nil {
		t.Fatal(err)
	}
	if err := fakeManager(t, path, "login-1").stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// realGUIDomain is a sample `launchctl print gui/501` output in the macOS format.
const realGUIDomain = "gui/501 = {\n\ttype = login\n\thandle = 101125\n\tcreator = loginwindow[93084]\n\tsecurity context = {\n\t\tuid = 501\n\t\tasid = 101125\n\t}\n}\n"

// TestLoginTokenNamesDomainIdentity pins that the login token is the identity of
// the domain's loginwindow creator, so a new login yields a new token.
func TestLoginTokenNamesDomainIdentity(t *testing.T) {
	run := func(context.Context, string, ...string) ([]byte, error) { return []byte(realGUIDomain), nil }
	identify := func(pid int) (Identity, error) { return Identity{PID: pid, StartToken: "boot-a"}, nil }
	got, err := loginToken(run, "gui/501", identify)(t.Context())
	if err != nil || got != "93084:boot-a" {
		t.Fatalf("token = %q, %v", got, err)
	}
}

// TestLoginTokenMissingCreatorFailsClosed pins that output without the creator line is an error.
func TestLoginTokenMissingCreatorFailsClosed(t *testing.T) {
	out := strings.Replace(realGUIDomain, "\tcreator = loginwindow[93084]\n", "", 1)
	run := func(context.Context, string, ...string) ([]byte, error) { return []byte(out), nil }
	identify := func(pid int) (Identity, error) { return Identity{PID: pid, StartToken: "x"}, nil }
	if got, err := loginToken(run, "gui/501", identify)(t.Context()); err == nil {
		t.Fatalf("token = %q without a creator line", got)
	}
}

// TestManagerStopSuccessLeavesStopped pins the clean-stop path: a manager that
// exits after stop recorded stopping makes start return nil and keep stopped.
func TestManagerStopSuccessLeavesStopped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	m := fakeManager(t, path, "login-1")
	m.launch = func(context.Context, []string) (Identity, func() error, error) {
		id := Identity{PID: 42, StartToken: "tok"}
		wait := func() error {
			return Save(path, State{Version: 1, State: "stopped", Host: "macbook", StopLoginSession: "login-1"})
		}
		return id, wait, nil
	}
	if err := m.start(context.Background(), []string{"manager"}); err != nil {
		t.Fatalf("clean stop returned error: %v", err)
	}
	got, err := Load(path)
	if err != nil || got.State != "stopped" {
		t.Fatalf("state = %+v, %v", got, err)
	}
}

// TestManagerLaunchFailureBlocks pins that a launch error is recorded as blocked with a reason.
func TestManagerLaunchFailureBlocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	m := fakeManager(t, path, "login-1")
	m.launch = func(context.Context, []string) (Identity, func() error, error) {
		return Identity{}, nil, errors.New("exec failed")
	}
	if err := m.start(context.Background(), []string{"manager"}); err == nil {
		t.Fatal("launch failure returned nil")
	}
	got, err := Load(path)
	if err != nil || got.State != "blocked" || got.Reason == "" {
		t.Fatalf("state = %+v, %v", got, err)
	}
}

// TestManagerReadyRecordsIdentityWhileRunning pins that the ready state carries
// the manager PID and token while the manager runs.
func TestManagerReadyRecordsIdentityWhileRunning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	m := fakeManager(t, path, "login-1")
	m.launch = func(context.Context, []string) (Identity, func() error, error) {
		wait := func() error {
			got, err := Load(path)
			if err != nil || got.State != "ready" || got.ManagerPID != 42 || got.ManagerToken != "tok" {
				t.Errorf("during run state = %+v, %v", got, err)
			}
			return Save(path, State{Version: 1, State: "stopped", Host: "macbook", StopLoginSession: "login-1"})
		}
		return Identity{PID: 42, StartToken: "tok"}, wait, nil
	}
	if err := m.start(context.Background(), []string{"manager"}); err != nil {
		t.Fatal(err)
	}
}

// TestManagerStopBlockedKeepsIdentity pins that a failed drain keeps the manager
// PID and token, so a later start is refused rather than launching a second manager.
func TestManagerStopBlockedKeepsIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Save(path, State{Version: 1, State: "ready", Host: "macbook", ManagerPID: 7, ManagerToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	m := fakeManager(t, path, "login-1")
	m.signal = func(int, syscall.Signal) error { return errors.New("EPERM") }
	if err := m.stop(context.Background()); err == nil {
		t.Fatal("stop returned nil after signal failure")
	}
	got, err := Load(path)
	if err != nil || got.State != "blocked" || got.ManagerPID != 7 || got.ManagerToken != "tok" {
		t.Fatalf("blocked state dropped identity: %+v, %v", got, err)
	}
	if err := m.start(context.Background(), []string{"manager"}); err == nil {
		t.Fatal("start launched past a live recorded manager")
	}
}

// TestLoginTokenRejectsNestedPid pins that a pid line outside the domain header
// block is not used as the login identity.
func TestLoginTokenRejectsNestedPid(t *testing.T) {
	run := func(context.Context, string, ...string) ([]byte, error) {
		return []byte("gui/501 = {\n\tcreator = {\n\t\tpid = 9\n\t}\n}\n"), nil
	}
	identify := func(pid int) (Identity, error) { return Identity{PID: pid, StartToken: "x"}, nil }
	if _, err := loginToken(run, "gui/501", identify)(context.Background()); err == nil {
		t.Fatal("nested pid accepted as login identity")
	}
}

// TestManagerStopDoesNotOverwriteConcurrentStart pins compare-and-set: when a
// start changes the state away from stopping before stop's final save, stop
// leaves the state alone and reports the change.
func TestManagerStopDoesNotOverwriteConcurrentStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Save(path, State{Version: 1, State: "ready", Host: "macbook", ManagerPID: 7, ManagerToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	m := fakeManager(t, path, "login-1")
	gone := false
	m.identify = func(pid int) (Identity, error) {
		if gone {
			return Identity{}, os.ErrNotExist
		}
		return Identity{PID: pid, StartToken: "tok"}, nil
	}
	m.signal = func(int, syscall.Signal) error {
		gone = true
		// A concurrent start lands here, after drain and before the final save.
		return Save(path, State{Version: 1, State: "ready", Host: "macbook", ManagerPID: 8, ManagerToken: "new"})
	}
	err := m.stop(context.Background())
	if err == nil {
		t.Fatal("stop overwrote a concurrent start")
	}
	got, loadErr := Load(path)
	if loadErr != nil || got.State != "ready" || got.ManagerPID != 8 {
		t.Fatalf("concurrent start clobbered: %+v, %v", got, loadErr)
	}
}

// TestManagerStartRefusedWhileStoppingThisLogin pins that a stopping record from
// this login refuses start while its manager is gone.
func TestManagerStartRefusedWhileStoppingThisLogin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Save(path, State{Version: 1, State: "stopping", Host: "macbook", StopLoginSession: "login-1", ManagerPID: 7, ManagerToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	m := fakeManager(t, path, "login-1")
	m.identify = func(int) (Identity, error) { return Identity{}, os.ErrNotExist }
	if err := m.start(context.Background(), []string{"manager"}); err == nil {
		t.Fatal("start succeeded while this login is stopping")
	}
}

// TestManagerWaitGoneHonoursCallerContext pins caller cancellation: an expired
// context returns DeadlineExceeded without waiting for the bound.
func TestManagerWaitGoneHonoursCallerContext(t *testing.T) {
	m := fakeManager(t, filepath.Join(t.TempDir(), "state.json"), "login-1")
	m.identify = func(pid int) (Identity, error) { return Identity{PID: pid, StartToken: "tok"}, nil }
	m.poll = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	if err := m.waitGone(ctx, 7, "tok", time.Hour); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitGone = %v, want DeadlineExceeded", err)
	}
}

// TestManagerDrainEscalatesSigtermThenSigkill pins escalation order: a manager
// that ignores SIGTERM gets SIGKILL after the grace, and a survivor is reported.
func TestManagerDrainEscalatesSigtermThenSigkill(t *testing.T) {
	m := fakeManager(t, filepath.Join(t.TempDir(), "state.json"), "login-1")
	var sent []syscall.Signal
	m.signal = func(_ int, sig syscall.Signal) error {
		sent = append(sent, sig)
		return nil
	}
	err := m.drain(context.Background(), 7, "tok")
	if err == nil || !strings.Contains(err.Error(), "survived SIGKILL") {
		t.Fatalf("drain = %v, want survived SIGKILL", err)
	}
	if len(sent) != 2 || sent[0] != syscall.SIGTERM || sent[1] != syscall.SIGKILL {
		t.Fatalf("signals = %v, want [SIGTERM SIGKILL]", sent)
	}
}

// TestManagerDrainSucceedsWhenKilled pins that a manager gone after SIGKILL is success.
func TestManagerDrainSucceedsWhenKilled(t *testing.T) {
	m := fakeManager(t, filepath.Join(t.TempDir(), "state.json"), "login-1")
	gone := false
	m.identify = func(pid int) (Identity, error) {
		if gone {
			return Identity{}, os.ErrNotExist
		}
		return Identity{PID: pid, StartToken: "tok"}, nil
	}
	m.signal = func(_ int, sig syscall.Signal) error {
		if sig == syscall.SIGKILL {
			gone = true
		}
		return nil
	}
	if err := m.drain(context.Background(), 7, "tok"); err != nil {
		t.Fatalf("drain after SIGKILL = %v, want nil", err)
	}
}

// TestManagerDrainNeverKillsReusedIdentity pins the pre-SIGKILL guard: the PID is
// the manager through the whole grace, then reused just before escalation.
func TestManagerDrainNeverKillsReusedIdentity(t *testing.T) {
	m := fakeManager(t, filepath.Join(t.TempDir(), "state.json"), "login-1")
	m.poll = time.Hour // waitGone checks once, then the grace deadline fires.
	var sent []syscall.Signal
	calls := 0
	m.identify = func(pid int) (Identity, error) {
		calls++
		if calls <= 2 {
			return Identity{PID: pid, StartToken: "tok"}, nil
		}
		return Identity{PID: pid, StartToken: "reused"}, nil
	}
	m.signal = func(_ int, sig syscall.Signal) error {
		sent = append(sent, sig)
		return nil
	}
	if err := m.drain(t.Context(), 7, "tok"); err != nil {
		t.Fatalf("drain = %v, want nil for a reused pid", err)
	}
	if calls != 3 || len(sent) != 1 || sent[0] != syscall.SIGTERM {
		t.Fatalf("identify calls = %d, signals = %v; want 3 and [SIGTERM]", calls, sent)
	}
}

// TestManagerAbandonDrainsUnrecordedChild pins that a failed ready save drains
// the child and records blocked with its identity.
func TestManagerAbandonDrainsUnrecordedChild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	m := fakeManager(t, path, "login-1")
	gone, waited := false, false
	m.launch = func(context.Context, []string) (Identity, func() error, error) {
		// A directory at the state path makes the ready save's rename fail.
		if err := os.Remove(path); err != nil {
			return Identity{}, nil, err
		}
		if err := os.Mkdir(path, 0700); err != nil {
			return Identity{}, nil, err
		}
		return Identity{PID: 42, StartToken: "tok"}, func() error { waited = true; return nil }, nil
	}
	m.identify = func(pid int) (Identity, error) {
		if gone {
			return Identity{}, os.ErrNotExist
		}
		return Identity{PID: pid, StartToken: "tok"}, nil
	}
	m.signal = func(_ int, sig syscall.Signal) error {
		gone = sig == syscall.SIGTERM
		return os.Remove(path)
	}
	if err := m.start(t.Context(), []string{"manager"}); err == nil {
		t.Fatal("start returned nil after the ready save failed")
	}
	got, err := Load(path)
	if !gone || !waited || err != nil || got.State != "blocked" || got.ManagerPID != 42 || got.ManagerToken != "tok" {
		t.Fatalf("gone=%v waited=%v state=%+v err=%v", gone, waited, got, err)
	}
}

// TestManagerExitDoesNotOverwriteConcurrentStop pins that start's blocked save
// waits for state.json.lock and keeps a stop that landed after the manager exited.
func TestManagerExitDoesNotOverwriteConcurrentStop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	m := fakeManager(t, path, "login-1")
	done := make(chan error, 1)
	m.launch = func(context.Context, []string) (Identity, func() error, error) {
		wait := func() error {
			lock, err := lockFile(t.Context(), path+".lock")
			if err != nil {
				return err
			}
			// The stop holds the lock as the manager exits, then records stopped.
			go func() {
				saveErr := Save(path, State{Version: 1, State: "stopped", Host: "macbook", StopLoginSession: "login-1"})
				done <- errors.Join(saveErr, lock.Close())
			}()
			return nil
		}
		return Identity{PID: 42, StartToken: "tok"}, wait, nil
	}
	err := m.start(t.Context(), []string{"manager"})
	if stopErr := <-done; stopErr != nil {
		t.Fatal(stopErr)
	}
	if err != nil {
		t.Fatalf("start after a stop = %v, want nil", err)
	}
	got, loadErr := Load(path)
	if loadErr != nil || got.State != "stopped" {
		t.Fatalf("stop overwritten: %+v, %v", got, loadErr)
	}
}

// TestManagerStopWaitsOnHeldLock pins the real lock: while another holder owns
// state.json.lock, stop returns DeadlineExceeded and leaves state untouched.
func TestManagerStopWaitsOnHeldLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Save(path, State{Version: 1, State: "ready", Host: "macbook", ManagerPID: 7, ManagerToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	held, err := lockFile(context.Background(), path+".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := fakeManager(t, path, "login-1").stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stop = %v, want DeadlineExceeded", err)
	}
	got, loadErr := Load(path)
	if loadErr != nil || got.State != "ready" || got.ManagerPID != 7 {
		t.Fatalf("state changed under held lock: %+v, %v", got, loadErr)
	}
}

// TestManagerStartWaitsOnHeldLock pins the same lock for start.
func TestManagerStartWaitsOnHeldLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	held, err := lockFile(context.Background(), path+".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := fakeManager(t, path, "login-1").start(ctx, []string{"manager"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("start = %v, want DeadlineExceeded", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state written under held lock: %v", err)
	}
}

// TestManagerStoppingFromEarlierLoginAllowsStart pins abandoned-stop recovery:
// a stopping record from an earlier login with a dead manager does not block start.
func TestManagerStoppingFromEarlierLoginAllowsStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Save(path, State{Version: 1, State: "stopping", Host: "macbook", StopLoginSession: "login-1", ManagerPID: 7, ManagerToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	m := fakeManager(t, path, "login-2")
	m.identify = func(int) (Identity, error) { return Identity{}, os.ErrNotExist }
	m.launch = func(context.Context, []string) (Identity, func() error, error) {
		return Identity{PID: 42, StartToken: "new"}, func() error {
			return Save(path, State{Version: 1, State: "stopped", Host: "macbook", StopLoginSession: "login-2"})
		}, nil
	}
	if err := m.start(context.Background(), []string{"manager"}); err != nil {
		t.Fatalf("start after abandoned stop = %v, want nil", err)
	}
}
