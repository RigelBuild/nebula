package cdgate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// scriptedProc is a process whose exit is driven by the test. exit is closed
// to let Wait return code.
type scriptedProc struct {
	pid     int
	code    int
	exit    chan struct{}
	dieOnce sync.Once
}

func (p *scriptedProc) PID() int { return p.pid }

func (p *scriptedProc) JoinOutput(context.Context) error { return nil }
func (p *scriptedProc) Wait() (int, error) {
	<-p.exit
	return p.code, nil
}

// die ends the process once. A real process exits once, so a second signal
// must not close the exit channel again.
func (p *scriptedProc) die() { p.dieOnce.Do(func() { close(p.exit) }) }

// scriptedRunner starts a scriptedProc and writes its scripted output into the
// sinks ExecGroup passes, as exec would copy a child's pipes.
type scriptedRunner struct {
	proc   *scriptedProc
	stdout string
	stderr string
}

func (r scriptedRunner) Start(_ []string, stdout, stderr io.Writer) (Process, error) {
	if _, err := io.WriteString(stdout, r.stdout); err != nil {
		return nil, err
	}
	if _, err := io.WriteString(stderr, r.stderr); err != nil {
		return nil, err
	}
	return r.proc, nil
}

// scriptedGroup tracks signals and liveness for one group. alive reports
// whether members remain; the test flips it to end the group.
type scriptedGroup struct {
	mu      sync.Mutex
	alive   bool
	signals []syscall.Signal
	onSig   func(sig syscall.Signal)
}

func (g *scriptedGroup) Alive(int) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.alive
}

func (g *scriptedGroup) Signal(_ int, sig syscall.Signal) error {
	g.mu.Lock()
	g.signals = append(g.signals, sig)
	cb := g.onSig
	g.mu.Unlock()
	if cb != nil {
		cb(sig)
	}
	return nil
}

func (g *scriptedGroup) sent() []syscall.Signal {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]syscall.Signal(nil), g.signals...)
}

// manualClock advances only when the test advances it. Sleep returns
// immediately after advancing, so no test waits on the wall clock.
type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func (c *manualClock) groupClock() GroupClock {
	return GroupClock{
		Now: c.Now,
		Sleep: func(ctx context.Context, d time.Duration) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			c.advance(d)
			return nil
		},
		// After never fires on its own. Only a test that wants the bounded-wait
		// timeout fires it, so the child-wait select cannot pick a ready timeout
		// over a child that is about to exit.
		After: func(time.Duration) <-chan time.Time { return make(chan time.Time) },
	}
}

func TestExecGroupReturnsCodeAndCapturedOutput(t *testing.T) {
	proc := &scriptedProc{pid: 9, code: 3, exit: make(chan struct{})}
	close(proc.exit)
	group := &scriptedGroup{}
	clk := &manualClock{now: time.Unix(0, 0)}
	runner := scriptedRunner{proc: proc, stdout: "stdout line\n", stderr: "stderr line\n"}
	var sinkOut, sinkErr bytes.Buffer
	capture := NewOrderedCapture()
	got, err := ExecGroup(context.Background(), runner, group, clk.groupClock(),
		[]string{"pulumi", "up"}, &sinkOut, &sinkErr, capture)
	if err != nil {
		t.Fatalf("ExecGroup: %v", err)
	}
	if got.Code != 3 || !strings.Contains(got.Output, "stdout line") || !strings.Contains(got.Output, "stderr line") {
		t.Fatalf("got = %+v, want code 3 and both streams captured", got)
	}
	if !strings.Contains(sinkOut.String(), "stdout line") || !strings.Contains(sinkErr.String(), "stderr line") {
		t.Fatalf("sinks = %q / %q, want the output reaching the real sinks", sinkOut.String(), sinkErr.String())
	}
	if len(group.sent()) != 0 {
		t.Fatalf("a normal exit signalled the group: %v", group.sent())
	}
}

func TestExecGroupStopSendsTermThenReturnsCode(t *testing.T) {
	proc := &scriptedProc{pid: 9, code: 143, exit: make(chan struct{})}
	group := &scriptedGroup{alive: false}
	group.onSig = func(syscall.Signal) { proc.die() }
	clk := &manualClock{now: time.Unix(0, 0)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := ExecGroup(ctx, scriptedRunner{proc: proc}, group, clk.groupClock(), []string{"x"}, io.Discard, io.Discard, nil)
	if err != nil {
		t.Fatalf("ExecGroup: %v", err)
	}
	if got.Code != 143 {
		t.Fatalf("code = %d, want the child's exit after a stop", got.Code)
	}
	if sent := group.sent(); len(sent) == 0 || sent[0] != syscall.SIGTERM {
		t.Fatalf("signals = %v, want SIGTERM first on stop", sent)
	}
}

func TestExecGroupWaitsForLiveDescendantAfterChildExit(t *testing.T) {
	// The direct child exits, but a background member keeps the group alive.
	// The executor must not return until the group is gone.
	proc := &scriptedProc{pid: 9, code: 0, exit: make(chan struct{})}
	close(proc.exit)
	group := &scriptedGroup{alive: true}
	clk := &manualClock{now: time.Unix(0, 0)}
	gc := clk.groupClock()
	var polls atomic.Int64
	gc.Sleep = func(ctx context.Context, d time.Duration) error {
		if polls.Add(1) == 3 {
			group.mu.Lock()
			group.alive = false
			group.mu.Unlock()
		}
		clk.advance(d)
		return nil
	}
	got, err := ExecGroup(context.Background(), scriptedRunner{proc: proc}, group, gc, []string{"x"}, io.Discard, io.Discard, nil)
	if err != nil {
		t.Fatalf("ExecGroup: %v", err)
	}
	if got.Code != 0 || polls.Load() < 3 {
		t.Fatalf("got = %+v, polls = %d; want to wait while the group lived", got, polls.Load())
	}
}

func TestExecGroupStuckGroupReturnsStuckError(t *testing.T) {
	// The child exits, but its group never does. Past GroupExitLimit the
	// executor must report stuck, so the caller keeps the Lease.
	proc := &scriptedProc{pid: 9, code: 0, exit: make(chan struct{})}
	close(proc.exit)
	group := &scriptedGroup{alive: true}
	clk := &manualClock{now: time.Unix(0, 0)}
	_, err := ExecGroup(context.Background(), scriptedRunner{proc: proc}, group, clk.groupClock(), []string{"x"}, io.Discard, io.Discard, nil)
	if _, ok := errors.AsType[*DeployGroupStuckError](err); !ok {
		t.Fatalf("err = %v, want DeployGroupStuckError for a group that never exits", err)
	}
	if sent := group.sent(); len(sent) == 0 || sent[0] != syscall.SIGTERM {
		t.Fatalf("signals = %v, want SIGTERM before reporting stuck", sent)
	}
}

func TestExecGroupStuckDirectChildDoesNotHang(t *testing.T) {
	// The child never exits after a stop. The bounded wait must return stuck
	// rather than block on Wait forever.
	proc := &scriptedProc{pid: 9, exit: make(chan struct{})}
	group := &scriptedGroup{alive: true}
	clk := &manualClock{now: time.Unix(0, 0)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	gc := clk.groupClock()
	gc.After = func(time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- clk.Now()
		return ch
	}
	_, err := ExecGroup(ctx, scriptedRunner{proc: proc}, group, gc, []string{"x"}, io.Discard, io.Discard, nil)
	if _, ok := errors.AsType[*DeployGroupStuckError](err); !ok {
		t.Fatalf("err = %v, want DeployGroupStuckError when the direct child never exits", err)
	}
}
