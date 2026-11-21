// SPDX-License-Identifier: MIT OR Apache-2.0

package cdgate

import (
	"context"
	"io"
	"syscall"
	"time"
)

// Group timing. SIGKILL follows SIGTERM after ChildKillGrace. A group that has
// not exited by GroupExitLimit is reported stuck. The lease loss timeout is
// 100s, so the group is dead before a contender's 120s takeover.
const (
	ChildKillGrace = 10 * time.Second
	GroupExitLimit = 30 * time.Second
	groupPoll      = 200 * time.Millisecond
)

// DeployGroupStuckError reports a deploy process group that did not exit after
// the stop. The caller keeps the Lease while a member that could still apply
// survives. Exited closes once the group is gone.
type DeployGroupStuckError struct {
	Exited <-chan struct{}
}

func (e *DeployGroupStuckError) Error() string { return "deploy process group did not exit" }

// Runner is the process seam the executor drives. The real one starts the
// command in its own process group. Tests substitute a scripted process.
type Runner interface {
	// Start runs argv in a new process group. stdout and stderr receive the
	// child's output; the runner never reads them itself.
	Start(argv []string, stdout, stderr io.Writer) (Process, error)
}

// Process is one running deploy group. Wait observes the direct child's exit;
// output-copying runners join their copies separately after the group stops.
type Process interface {
	// PID is the group id.
	PID() int
	// Wait returns the direct child's exit code.
	Wait() (int, error)
	// JoinOutput joins copied stdout and stderr once the process group is stopped.
	JoinOutput(context.Context) error
}

// GroupProbe reports whether a process group still has live members, and
// delivers a signal to it. It is SignalGroup with its process seams injected.
type GroupProbe interface {
	// Alive reports whether any member of the group is still running.
	Alive(pid int) bool
	// Signal sends sig to the group. An error other than a missing group is
	// returned to the caller.
	Signal(pid int, sig syscall.Signal) error
}

// GroupClock supplies the time functions used by process-group monitoring.
type GroupClock struct {
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
	// After returns a channel that fires once d has elapsed.
	After func(d time.Duration) <-chan time.Time
}

// StepOutput is one deploy step's result, as the retry classifier reads it.
type StepOutput struct {
	Code     int
	Output   string
	Overflow bool
}

// ExecGroup runs argv in a process group and returns its exit code and output.
// A stop (ctx done) sends SIGTERM, then SIGKILL after ChildKillGrace. It keeps
// waiting while any group member is alive. Past GroupExitLimit it returns
// DeployGroupStuckError, so the caller holds the Lease.
func ExecGroup(ctx context.Context, runner Runner, probe GroupProbe, clock GroupClock, argv []string, stdout, stderr io.Writer, capture *OrderedCapture) (StepOutput, error) {
	// The stop path sleeps after the caller's ctx is cancelled, so it keeps the
	// caller's values but drops its cancellation.
	stopCtx := context.WithoutCancel(ctx)
	outW, errW := stdout, stderr
	if capture != nil {
		outW = io.MultiWriter(stdout, capture.Stdout)
		errW = io.MultiWriter(stderr, capture.Stderr)
	}
	proc, err := runner.Start(argv, outW, errW)
	if err != nil {
		return StepOutput{}, err
	}

	exitCh := make(chan exitResult, 1)
	go func() {
		code, waitErr := proc.Wait()
		exitCh <- exitResult{code: code, err: waitErr}
	}()

	var (
		stopping         bool
		stopDeadline     time.Time
		escalationCancel context.CancelFunc
	)
	stop := func() {
		if stopping {
			return
		}
		stopping = true
		stopDeadline = clock.Now().Add(GroupExitLimit)
		// The signal is best effort: a group that already exited is not an error,
		// and a survivor is caught by the liveness loop below.
		_ = probe.Signal(proc.PID(), syscall.SIGTERM)
		escalationCtx, cancel := context.WithCancel(stopCtx)
		escalationCancel = cancel
		go func() {
			if clock.Sleep(escalationCtx, ChildKillGrace) == nil {
				_ = probe.Signal(proc.PID(), syscall.SIGKILL)
			}
		}()
	}
	defer func() {
		if escalationCancel != nil {
			escalationCancel()
		}
	}()

	var res exitResult
	select {
	case res = <-exitCh:
	case <-ctx.Done():
		stop()
		select {
		case res = <-exitCh:
		case <-clock.After(GroupExitLimit):
			return StepOutput{}, &DeployGroupStuckError{Exited: groupGone(stopCtx, probe, proc.PID(), clock)}
		}
	}
	if res.err != nil {
		return StepOutput{}, res.err
	}

	// A background descendant can outlive a shell that exits normally. It must
	// not keep applying after the Lease is released, so the group is awaited.
	for probe.Alive(proc.PID()) {
		stop()
		if !clock.Now().Before(stopDeadline) {
			return StepOutput{Code: res.code}, &DeployGroupStuckError{Exited: groupGone(stopCtx, probe, proc.PID(), clock)}
		}
		if err := clock.Sleep(stopCtx, groupPoll); err != nil {
			return StepOutput{}, err
		}
	}

	joinCtx, cancel := context.WithTimeout(stopCtx, GroupExitLimit)
	defer cancel()
	if err := proc.JoinOutput(joinCtx); err != nil {
		return StepOutput{}, err
	}
	var combined []byte
	var overflow bool
	if capture != nil {
		combined, overflow = capture.Result()
	}
	return StepOutput{Code: res.code, Output: string(combined), Overflow: overflow}, nil
}

type exitResult struct {
	code int
	err  error
}

// groupGone returns a channel that closes once the group has no members.
func groupGone(ctx context.Context, probe GroupProbe, pid int, clock GroupClock) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		for probe.Alive(pid) {
			if err := clock.Sleep(ctx, groupPoll); err != nil {
				return
			}
		}
		close(done)
	}()
	return done
}
