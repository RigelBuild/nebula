package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPickRunnerHostFreeHostDoesNotTicketOtherHost(t *testing.T) {
	skipUnlessLinux(t)
	f, runner := sshGateFixture(t)
	runner.hosts = []string{"busy@fixture", "free@fixture"}
	bin := filepath.Join(os.Getenv("HOME"), "bin", "ssh")
	writeFakeTool(t, bin, `#!/bin/sh
while [ "$1" = -o ] || [ "$1" = -i ]; do shift 2; done
shift
host=$1; shift
HOME="$HOME/$host"; export HOME
mkdir -p "$HOME"
exec sh -c "$*"
`)
	busy := filepath.Join(os.Getenv("HOME"), "busy@fixture", "rjc-hook", "slots", "s1")
	holdQueueFile(t, busy)
	t.Setenv("RJC_HOOK_SSH_SLOTS", "1")
	picked, _, err := runner.pickRunnerHost(f.ctx, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if picked.host != "free@fixture" {
		t.Fatalf("picked %s", picked.host)
	}
	entries, err := os.ReadDir(filepath.Join(filepath.Dir(filepath.Dir(busy)), "queue"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != ".lock" {
			t.Fatalf("unexpected ticket: %s", entry.Name())
		}
	}
}

// queueLogWriter cancels its context once the waiting log reports the expected position.
type queueLogWriter struct {
	cancel context.CancelFunc
	buffer bytes.Buffer
}

func (w *queueLogWriter) Write(p []byte) (int, error) {
	n, err := w.buffer.Write(p)
	if strings.Contains(w.buffer.String(), "position 2 of 2") {
		w.cancel()
	}
	return n, err
}

// TestPickRunnerHostLogsQueuePosition: with one live ticket ahead of ours and a full host, the wait log names our position.
func TestPickRunnerHostLogsQueuePosition(t *testing.T) {
	skipUnlessLinux(t)
	f, runner := sshGateFixture(t)
	t.Setenv("RJC_HOOK_SSH_TIMEOUT", "5m")
	runner.hosts = []string{"gate@fixture.invalid"}
	ahead := filepath.Join(os.Getenv("HOME"), "rjc-hook", "queue", "00000000000000000001-ahead")
	createQueueTicket(t, ahead)
	for slot := 1; slot <= defaultSSHSlots; slot++ {
		holdQueueFile(t, filepath.Join(os.Getenv("HOME"), "rjc-hook", "slots", "s"+strconv.Itoa(slot)))
	}
	ctx, cancel := context.WithTimeout(f.ctx, 15*time.Second)
	defer cancel()
	log := &queueLogWriter{cancel: cancel}
	if _, _, err := runner.pickRunnerHost(ctx, log); err == nil {
		t.Fatal("a full host should keep the queue waiting until the context is canceled")
	}
	if !strings.Contains(log.buffer.String(), "hook-ssh: waiting for a gate slot, position 2 of 2 (hosts: ") {
		t.Fatalf("waiting log has the wrong queue-position format: %q", log.buffer.String())
	}
	entries, err := os.ReadDir(filepath.Dir(ahead))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != ".lock" && entry.Name() != filepath.Base(ahead) {
			t.Fatalf("waiting client left ticket %q after cancellation", entry.Name())
		}
	}
}

func TestAcquireLocalGateSlotWaitsForThirdRun(t *testing.T) {
	base := t.TempDir()
	env := []string{"XDG_RUNTIME_DIR=" + base, "GATE_SLOTS=2"}
	log := &strings.Builder{}
	releases := make([]func() error, 0, 2)
	for range 2 {
		release, err := acquireLocalGateSlot(t.Context(), env, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	for _, release := range releases {
		defer func() {
			if err := release(); err != nil {
				t.Error(err)
			}
		}()
	}
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()
	if _, err := acquireLocalGateSlot(ctx, env, log); err == nil {
		t.Fatal("third run acquired a slot while both slots were held")
	} else if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("third run error = %v, want context deadline exceeded", err)
	}
	if !strings.Contains(log.String(), "waiting for a local gate slot") {
		t.Fatalf("waiting progress = %q", log.String())
	}
}
func TestRunHooksQueuesEachChangeBeforeTransfer(t *testing.T) {
	skipUnlessLinux(t)
	f, runner := sshGateFixture(t)
	t.Setenv("RJC_HOOK_SSH_SLOTS", "1")
	f.commit(t, "hk.pkl")
	base, first := sshRange(t, f)
	f.commit(t, "second.txt")
	_, second := sshRange(t, f)
	line := []Change{{Tip: first, Base: base, Owned: true}, {Tip: second, Base: first, Owned: true}}
	runner.hosts = []string{"gate@fixture.invalid"}
	queue := filepath.Join(os.Getenv("HOME"), "rjc-hook", "queue")
	waiter := filepath.Join(queue, "00000000000000000001-waiter")
	createQueueTicket(t, waiter)
	marker := filepath.Join(t.TempDir(), "events")
	bin := filepath.Join(os.Getenv("HOME"), "bin", "ssh")
	writeFakeTool(t, bin, strings.Replace(fakeSSH, "sh -c \"$*\" < \"$in\" 3<&-\nrc=$?", `case "$*" in
  *"read -r load"*) printf 'probe\n' >> "$RJC_TEST_QUEUE_EVENTS" ;;
  *"hk run pre-push"*) printf 'transfer\n' >> "$RJC_TEST_QUEUE_EVENTS" ;;
esac
sh -c "$*" < "$in" 3<&-
rc=$?
case "$*" in *"hk run pre-push"*) printf 'exit %s\n' "$rc" >> "$RJC_TEST_QUEUE_EVENTS" ;; esac`, 1))
	t.Setenv("RJC_TEST_QUEUE_EVENTS", marker)
	waits := 0
	runner.wait = func(_ context.Context, _ time.Duration) error {
		waits++
		return os.Remove(waiter)
	}
	if err := f.repo.runHooks(f.ctx, line, nil, runner, localPrePush{root: f.repo.stateDir}); err != nil {
		t.Fatal(err)
	}
	if waits != 1 {
		t.Fatalf("wait count = %d, want 1", waits)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	events := strings.Fields(string(data))
	transfers := 0
	probes := 0
	for i, event := range events {
		if event == "probe" {
			probes++
		}
		if event == "transfer" {
			if probes == 0 {
				t.Fatalf("transfer without preceding probe: %q", data)
			}
			if i+2 >= len(events) || events[i+1] != "exit" || events[i+2] != "0" {
				t.Fatalf("transfer refused or failed: %q", data)
			}
			probes = 0
			transfers++
		}
	}
	if transfers != 2 {
		t.Fatalf("transfer count = %d: %q", transfers, data)
	}
}
func TestHeartbeatFollowsSelectedHostAndStops(t *testing.T) {
	f, runner := sshGateFixture(t)
	root := t.TempDir()
	for _, host := range []string{"first", "second"} {
		createQueueTicket(t, filepath.Join(root, host, "rjc-hook", "queue", "00000000000000000001-id"))
	}
	writeFakeTool(t, filepath.Join(os.Getenv("HOME"), "bin", "ssh"), `#!/bin/sh
while [ "$1" = -o ] || [ "$1" = -i ]; do shift 2; done
printf 'call\n' >> "$RJC_HEARTBEAT_EVENTS"
shift; host=$1; shift
HOME="$RJC_HEARTBEAT_ROOT/$host"; export HOME
exec sh -c "$*"
`)
	t.Setenv("RJC_HEARTBEAT_ROOT", root)
	events := filepath.Join(root, "calls")
	t.Setenv("RJC_HEARTBEAT_EVENTS", events)
	runner.queue = &queueHosts{}
	runner.ticket = "id"
	for _, host := range []string{"first", "second"} {
		path := filepath.Join(root, host, "rjc-hook", "queue", "00000000000000000001-id")
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		runner.host = host
		stop := runner.heartbeatTicket(f.ctx, time.Millisecond)
		deadline := time.NewTimer(5 * time.Second)
		for {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.ModTime().After(old) {
				break
			}
			select {
			case <-deadline.C:
				t.Fatal("heartbeat did not touch ", host)
			case <-time.After(time.Millisecond):
			}
		}
		deadline.Stop()
		stop()
		before, err := os.Stat(events)
		if err != nil {
			t.Fatal(err)
		}
		<-time.After(100 * time.Millisecond)
		after, err := os.Stat(events)
		if err != nil || after.Size() != before.Size() {
			t.Fatalf("heartbeat invoked ssh after stop: %v", err)
		}
	}
}

func TestHeartbeatStopsWhenTicketMissing(t *testing.T) {
	f, runner := sshGateFixture(t)
	root := t.TempDir()
	events := filepath.Join(root, "calls")
	t.Setenv("RJC_HEARTBEAT_EVENTS", events)
	t.Setenv("RJC_HEARTBEAT_HOME", root)
	writeFakeTool(t, filepath.Join(os.Getenv("HOME"), "bin", "ssh"), `#!/bin/sh
while [ "$1" = -o ] || [ "$1" = -i ]; do shift 2; done
printf 'call\n' >> "$RJC_HEARTBEAT_EVENTS"
shift; shift
HOME="$RJC_HEARTBEAT_HOME"; export HOME
exec sh -c "$*"
`)
	runner.queue = &queueHosts{}
	runner.ticket = "missing"
	stop := runner.heartbeatTicket(f.ctx, time.Millisecond)
	defer stop()
	<-time.After(100 * time.Millisecond)
	stop()
	data, err := os.ReadFile(events)
	if err != nil {
		t.Fatal(err)
	}
	if calls := strings.Count(string(data), "call\n"); calls != 1 {
		t.Fatalf("ssh calls = %d, want 1", calls)
	}
}
func TestRunRepickMovesHeartbeatToNewHost(t *testing.T) {
	skipUnlessLinux(t)
	f, runner := sshGateFixture(t)
	f.commit(t, "hk.pkl")
	base, head := sshRange(t, f)
	home := os.Getenv("HOME")
	ticket := "repick"
	for _, host := range []string{"host-a", "host-b"} {
		createQueueTicket(t, filepath.Join(home, host, "rjc-hook", "queue", "00000000000000000001-"+ticket))
	}
	events := filepath.Join(home, "events")
	t.Setenv("RJC_REPICK_EVENTS", events)
	t.Setenv("RJC_REPICK_HOME", home)
	writeFakeTool(t, filepath.Join(home, "bin", "ssh"), `#!/bin/sh
while [ "$1" = -o ] || [ "$1" = -i ]; do shift 2; done
shift; host=$1; shift
case "$*" in
  *" heartbeat "*) printf '%s\n' "$host" >> "$RJC_REPICK_EVENTS"; HOME="$RJC_REPICK_HOME/$host"; export HOME; exec sh -c "$*" ;;
  *"for-each-ref"*) if [ "$host" = host-b ]; then sleep 1; fi; exit 0 ;;
  *"hk run pre-push"*) if [ "$host" = host-a ]; then exit 77; fi ;;
esac
HOME="$RJC_REPICK_HOME/$host"; export HOME
exec sh -c "$*"
`)
	runner.host = "host-a"
	runner.hosts = []string{"host-b"}
	runner.ticket = ticket
	runner.queue = &queueHosts{}
	runner.heartbeatInterval = 10 * time.Millisecond
	runner.probe = func(context.Context, string) (hostStatus, error) {
		return hostStatus{nproc: 1, freeGiB: 100, freeSlots: 1}, nil
	}
	_, _, err := runner.Run(f.ctx, base, head, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(events)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(string(data))
	foundB := false
	for _, host := range lines {
		if host == "host-b" {
			foundB = true
		}
		if foundB && host == "host-a" {
			t.Fatalf("old host touched after repick: %q", data)
		}
	}
	if !foundB {
		t.Fatalf("new host not heartbeated during prep: %q", data)
	}
}
