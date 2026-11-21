package command

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec" //nolint:depguard // Fixed argv runs directly, never through a shell the watchdog builds.
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	CommandTimeout = 30 * time.Second
	MappingTimeout = 120 * time.Second
	maxOutputBytes = 64 << 20
	killGrace      = time.Second
)

// Result carries the text a command wrote to each stream.
type Result struct {
	Stdout string
	Stderr string
}

// Error reports a child that exited non-zero or by signal.
type Error struct {
	Status string
	Stderr string
}

func (e *Error) Error() string {
	return fmt.Sprintf("command exited with status %s; stderr=%s", e.Status, e.Stderr)
}

type TimeoutError struct {
	timeout time.Duration
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("command timed out after %gs", e.timeout.Seconds())
}

var errOutputExceeded = errors.New("command output exceeded maxBuffer")

// boundedBuffer keeps the first maxOutputBytes and discards the rest, so the child never
// blocks on a full pipe. The first overflow calls onOverflow once.
type boundedBuffer struct {
	mu         sync.Mutex
	buf        bytes.Buffer
	total      int
	overflowed bool
	onOverflow func()
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.total += len(p)
	if b.total > maxOutputBytes {
		if !b.overflowed {
			b.overflowed = true
			b.onOverflow()
		}
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *boundedBuffer) snapshot() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String(), b.overflowed
}

// Text runs a command in its own process group. A timeout, output overflow, or
// cancelled ctx SIGKILLs the group, then settles even if an escaped process still holds
// the pipes. A stop error takes precedence over the exit status.
func Text(ctx context.Context, name string, args []string, timeout time.Duration) (Result, error) {
	overflow := make(chan struct{})
	var overflowOnce sync.Once
	onOverflow := func() { overflowOnce.Do(func() { close(overflow) }) }
	stdout := &boundedBuffer{onOverflow: onOverflow}
	stderr := &boundedBuffer{onOverflow: onOverflow}

	cmd := exec.Command(name, args...) //nolint:gosec // G204: name is a fixed binary from the unit PATH (env, runuser, ss, bash, find), never caller input.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		errText, _ := stderr.snapshot()
		return Result{}, fmt.Errorf("%w; stderr=%s", err, strings.TrimSpace(errText))
	}
	pid := cmd.Process.Pid
	var reaped atomic.Bool
	waitCh := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		reaped.Store(true)
		waitCh <- err
	}()

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var stop error
	select {
	case err := <-waitCh:
		return finishCommand(err, stdout, stderr, nil)
	case <-runCtx.Done():
		stop = stopError(ctx, timeout)
	case <-overflow:
		stop = errOutputExceeded
	}
	if killErr := killGroup(pid, &reaped); killErr != nil {
		stop = errors.Join(stop, killErr)
	}
	var waitErr error
	select {
	case waitErr = <-waitCh:
	case <-time.After(killGrace):
		// An escaped descendant still holds the pipes; settle with what was captured.
	}
	// The stop error takes precedence over the killed child's exit status.
	// Any other wait failure is added to it.
	if waitErr != nil {
		if _, isExit := errors.AsType[*exec.ExitError](waitErr); !isExit {
			stop = errors.Join(stop, waitErr)
		}
	}
	return captured(stdout, stderr), stop
}

// stopError names why the run stopped: the caller's cancel, or our own timeout.
func stopError(parent context.Context, timeout time.Duration) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	return &TimeoutError{timeout: timeout}
}

func captured(stdout, stderr *boundedBuffer) Result {
	text, _ := stdout.snapshot()
	errText, _ := stderr.snapshot()
	return Result{Stdout: text, Stderr: errText}
}

// finishCommand maps a finished child to its result and error.
func finishCommand(err error, stdout, stderr *boundedBuffer, stop error) (Result, error) {
	result := captured(stdout, stderr)
	if _, overflowed := stdout.snapshot(); overflowed {
		return result, errOutputExceeded
	}
	if _, overflowed := stderr.snapshot(); overflowed {
		return result, errOutputExceeded
	}
	if stop != nil {
		return result, stop
	}
	if err == nil {
		return result, nil
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		status := strconv.Itoa(exitErr.ExitCode())
		if signal := exitSignal(exitErr); signal != "" {
			status = signal
		}
		return result, &Error{Status: status, Stderr: strings.TrimSpace(result.Stderr)}
	}
	return result, fmt.Errorf("%w; stderr=%s", err, strings.TrimSpace(result.Stderr))
}

// exitSignal names a fatal signal as POSIX does (SIGKILL, not Go's "killed"), or returns "" for a normal exit.
func exitSignal(err *exec.ExitError) string {
	status, ok := err.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return ""
	}
	if name, known := signalNames[status.Signal()]; known {
		return name
	}
	return "signal " + strconv.Itoa(int(status.Signal()))
}

// signalNames maps each fatal signal a watched child can die by to the SIG* name the relaunch log and tests match on.
var signalNames = map[syscall.Signal]string{
	syscall.SIGHUP:  "SIGHUP",
	syscall.SIGINT:  "SIGINT",
	syscall.SIGQUIT: "SIGQUIT",
	syscall.SIGABRT: "SIGABRT",
	syscall.SIGKILL: "SIGKILL",
	syscall.SIGSEGV: "SIGSEGV",
	syscall.SIGPIPE: "SIGPIPE",
	syscall.SIGALRM: "SIGALRM",
	syscall.SIGTERM: "SIGTERM",
}

// signalGroup sends SIGKILL to the group. ESRCH and EPERM are not fatal: the leader is still
// awaited, and the caller settles on Wait or the grace timer.
func signalGroup(pid int) error {
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if err == nil || errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM) {
		return nil
	}
	return err
}
