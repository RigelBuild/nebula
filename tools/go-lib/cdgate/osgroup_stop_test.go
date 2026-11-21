package cdgate

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// recordingProbe wraps the real probe and remembers the group ids it was asked
// about, so the test can check that exact group afterwards.
type recordingProbe struct {
	OSGroupProbe
	mu      sync.Mutex
	pids    []int
	signals []syscall.Signal
}

func (p *recordingProbe) Signal(pid int, sig syscall.Signal) error {
	p.mu.Lock()
	p.pids = append(p.pids, pid)
	p.signals = append(p.signals, sig)
	p.mu.Unlock()
	return p.OSGroupProbe.Signal(pid, sig)
}

// TestOSGroupStopReachesDescendant starts a shell that backgrounds a sleeper in
// the same group, stops the step, and asserts the group the test started has no
// live members afterwards. Only the group this test created is signalled.
func TestOSGroupStopReachesDescendant(t *testing.T) {
	runner := OSRunner{Env: []string{"PATH=" + os.Getenv("PATH")}}
	clk := GroupClock{
		Now:   time.Now,
		Sleep: sleepOrDone,
		After: time.After,
	}
	probe := &recordingProbe{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		out StepOutput
		err error
	}
	done := make(chan result, 1)
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("create readiness pipe: %v", err)
	}
	defer readyRead.Close()
	defer readyWrite.Close()
	stdout := io.MultiWriter(readyWrite, io.Discard)
	go func() {
		out, err := ExecGroup(ctx, runner, probe, clk,
			[]string{"/bin/sh", "-c", `sleep 300 & child=$!; printf "%s\n" "$child"; wait "$child"`}, stdout, io.Discard, nil)
		done <- result{out, err}
	}()
	type readiness struct {
		pid int
		err error
	}
	ready := make(chan readiness, 1)
	go func() {
		scanner := bufio.NewScanner(readyRead)
		if scanner.Scan() {
			pid, err := strconv.Atoi(scanner.Text())
			ready <- readiness{pid: pid, err: err}
			return
		}
		if err := scanner.Err(); err != nil {
			ready <- readiness{err: err}
			return
		}
		ready <- readiness{err: errors.New("readiness pipe closed before pid")}
	}()
	readyDeadline := time.NewTimer(10 * time.Second)
	defer readyDeadline.Stop()
	var descendantPID int
	select {
	case r := <-ready:
		if r.err != nil {
			t.Fatalf("read descendant readiness: %v", r.err)
		}
		if r.pid <= 0 {
			t.Fatalf("invalid descendant pid: %d", r.pid)
		}
		descendantPID = r.pid
	case r := <-done:
		t.Fatalf("ExecGroup returned before descendant readiness: %+v", r)
	case <-readyDeadline.C:
		cancel()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatal("ExecGroup did not return after readiness timed out")
		}
		t.Fatal("descendant did not signal readiness")
	}
	descendantPGID, err := syscall.Getpgid(descendantPID)
	if err != nil {
		t.Fatalf("get descendant process group: %v", err)
	}
	cancel()
	resultDeadline := time.NewTimer(20 * time.Second)
	defer resultDeadline.Stop()
	var r result
	select {
	case r = <-done:
	case <-resultDeadline.C:
		t.Fatal("ExecGroup did not return after a stop: the group was not signalled")
	}
	if r.err != nil {
		t.Fatalf("ExecGroup: %v", r.err)
	}
	probe.mu.Lock()
	pids := append([]int(nil), probe.pids...)
	signals := append([]syscall.Signal(nil), probe.signals...)
	probe.mu.Unlock()
	if len(pids) == 0 || len(signals) == 0 || signals[0] != syscall.SIGTERM {
		t.Fatalf("signals = %v for groups %v, want SIGTERM to stop the group", signals, pids)
	}
	if descendantPGID != pids[0] {
		t.Fatalf("descendant process group = %d, signalled group = %d; want one process group", descendantPGID, pids[0])
	}
	livenessTicker := time.NewTicker(10 * time.Millisecond)
	defer livenessTicker.Stop()
	livenessDeadline := time.NewTimer(10 * time.Second)
	defer livenessDeadline.Stop()
	for (OSGroupProbe{}).Alive(descendantPGID) {
		select {
		case <-livenessTicker.C:
		case <-livenessDeadline.C:
			t.Fatalf("group %d still has live members after the stop: the descendant survived", descendantPGID)
		}
	}
}

func TestOSGroupCaptureStopsBackgroundDescendant(t *testing.T) {
	runner := OSRunner{Env: []string{"PATH=" + os.Getenv("PATH")}}
	clk := GroupClock{Now: time.Now, Sleep: sleepOrDone, After: time.After}
	probe := &recordingProbe{}
	capture := NewOrderedCapture()
	done := make(chan struct {
		out StepOutput
		err error
	}, 1)
	go func() {
		out, err := ExecGroup(context.Background(), runner, probe, clk,
			[]string{"/bin/sh", "-c", "sleep 300 & echo started"}, io.Discard, io.Discard, capture)
		done <- struct {
			out StepOutput
			err error
		}{out: out, err: err}
	}()

	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("ExecGroup: %v", result.err)
		}
		if result.out.Code != 0 || !strings.Contains(result.out.Output, "started") {
			t.Fatalf("result = %+v, want successful captured output", result.out)
		}
	case <-deadline.C:
		t.Fatal("ExecGroup did not return after stopping the background descendant")
	}
	probe.mu.Lock()
	pids := append([]int(nil), probe.pids...)
	probe.mu.Unlock()
	if len(pids) == 0 || (OSGroupProbe{}).Alive(pids[0]) {
		t.Fatalf("process group = %v, want no live group after ExecGroup returned", pids)
	}
}
