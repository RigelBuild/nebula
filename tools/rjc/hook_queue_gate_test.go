package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// holdQueueFile holds a slot flock in a child process until the test ends.
func holdQueueFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("flock", path, "sh", "-c", "echo locked; read _ || :")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := stdin.Close(); err != nil {
			t.Errorf("close lock holder stdin: %v", err)
		}
		if err := cmd.Wait(); err != nil {
			t.Errorf("wait for lock holder: %v", err)
		}
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "locked" {
		t.Fatalf("lock holder did not acquire %s: %q, %v", path, line, err)
	}
}

func createQueueTicket(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// runQueueGate runs the real remote run script for one ticket. The ticket travels in RJC_HOOK_TICKET, as the
// client sends it on the remote command line. It returns the exit status, so a slot-busy refusal (77) is visible.
func runQueueGate(t *testing.T, home, ticket, orderPath string) (int, string) {
	t.Helper()
	dir := t.TempDir()
	// Simulate the real slot script handoff after flock acquires the slot.
	slotScript := `if [ -n "$queue_file" ]; then rm -f "$queue_file"; fi; flock -u 8; printf '%s\n' "$RJC_HOOK_TICKET" >> "$RJC_TEST_QUEUE_ORDER"`
	args := []string{"-c", sshHookRun, "rjc-hook", dir, filepath.Join(home, "repo"), filepath.Join(home, "repo.git"), "base", "head", "0", "1", slotScript, ":", ":", "hk", "hk.pkl", "30s"}
	cmd := exec.Command("sh", args...)
	cmd.Env = append(os.Environ(), "HOME="+home, "RJC_HOOK_TICKET="+ticket, "RJC_TEST_QUEUE_ORDER="+orderPath)
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return exit.ExitCode(), string(out)
	}
	t.Fatalf("run ssh hook gate: %v: %s", err, out)
	return -1, string(out)
}

// TestSSHHookScriptsRespectHostSlotLimit verifies both the remote run and host-probe scripts.
func TestSSHHookScriptsRespectHostSlotLimit(t *testing.T) {
	requireGateHostShell(t)
	tests := []struct {
		name            string
		hostSlots       string
		wantSlots       string
		wantBusyRunCode int
		wantProbeFree   int
	}{
		{name: "host cap", hostSlots: "1", wantSlots: "1", wantBusyRunCode: sshSlotBusy, wantProbeFree: 0},
		{name: "host cap unset", wantSlots: "2", wantBusyRunCode: 0, wantProbeFree: 1},
		{name: "host cap zero", hostSlots: "0", wantSlots: "2", wantBusyRunCode: 0, wantProbeFree: 1},
		{name: "invalid host cap", hostSlots: "two", wantSlots: "2", wantBusyRunCode: 0, wantProbeFree: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			env := make([]string, 0, len(os.Environ())+2)
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "HOME=") && !strings.HasPrefix(entry, "RJC_HOOK_SSH_HOST_SLOTS=") && !strings.HasPrefix(entry, "RJC_HOOK_TICKET=") {
					env = append(env, entry)
				}
			}
			env = append(env, "HOME="+home)
			if tt.hostSlots != "" {
				env = append(env, "RJC_HOOK_SSH_HOST_SLOTS="+tt.hostSlots)
			}

			run := func(mark string) (int, string) {
				slotScript := "printf '%s' \"$RJC_HOOK_SSH_SLOTS\" > " + shQuote(mark)
				args := []string{
					"-c", sshHookRun, "rjc-hook", t.TempDir(), filepath.Join(home, "repo"), filepath.Join(home, "repo.git"),
					"base", "head", "0", "2", slotScript, ":", ":", "hk", "hk.pkl", "30s",
				}
				cmd := exec.Command("sh", args...)
				cmd.Env = append(slices.Clip(env), "RJC_HOOK_TICKET=")
				cmd.Stdin = strings.NewReader("")
				out, err := cmd.CombinedOutput()
				if err == nil {
					return 0, string(out)
				}
				if exit, ok := errors.AsType[*exec.ExitError](err); ok {
					return exit.ExitCode(), string(out)
				}
				t.Fatalf("run ssh hook script: %v: %s", err, out)
				return -1, string(out)
			}

			mark := filepath.Join(home, "run-slots")
			if code, output := run(mark); code != 0 {
				t.Fatalf("initial run exited %d: %s", code, output)
			}
			gotSlots, err := os.ReadFile(mark)
			if err != nil {
				t.Fatal(err)
			}
			if string(gotSlots) != tt.wantSlots {
				t.Fatalf("run script exported RJC_HOOK_SSH_SLOTS=%q, want %q", gotSlots, tt.wantSlots)
			}

			holdQueueFile(t, filepath.Join(home, "rjc-hook", "slots", "s1"))
			mark = filepath.Join(home, "busy-run-slots")
			if code, output := run(mark); code != tt.wantBusyRunCode {
				t.Fatalf("run with slot s1 held exited %d, want %d: %s", code, tt.wantBusyRunCode, output)
			}
			if tt.wantBusyRunCode == 0 {
				gotSlots, err = os.ReadFile(mark)
				if err != nil {
					t.Fatal(err)
				}
				if string(gotSlots) != tt.wantSlots {
					t.Fatalf("run script exported RJC_HOOK_SSH_SLOTS=%q, want %q", gotSlots, tt.wantSlots)
				}
			} else if _, err := os.Stat(mark); !os.IsNotExist(err) {
				t.Fatalf("busy run marker stat error = %v, want no marker", err)
			}

			probe := exec.Command("sh", "-c", sshHostProbeScript, "rjc-probe", "2")
			probe.Env = env
			out, err := probe.Output()
			if err != nil {
				t.Fatalf("run host probe script: %v", err)
			}
			status, err := parseHostStatus(string(out))
			if err != nil {
				t.Fatalf("parse host probe output %q: %v", out, err)
			}
			if status.freeSlots != tt.wantProbeFree {
				t.Fatalf("probe free slots = %d, want %d", status.freeSlots, tt.wantProbeFree)
			}
		})
	}
}

// TestSSHHookGateRunsQueuedTicketsInArrivalOrder: three tickets wait in arrival order, and each run gets the slot in that order.
func TestSSHHookGateRunsQueuedTicketsInArrivalOrder(t *testing.T) {
	requireGateHostShell(t)
	home := t.TempDir()
	queue := filepath.Join(home, "rjc-hook", "queue")
	orderPath := filepath.Join(home, "order")
	tickets := []string{"first", "second", "third"}
	for i, ticket := range tickets {
		createQueueTicket(t, filepath.Join(queue, fmt.Sprintf("%020d-%s", i+1, ticket)))
	}
	// Tickets 2 and 3 are not first in line, so they must be refused while ticket 1 is live.
	for _, index := range []int{2, 1} {
		code, output := runQueueGate(t, home, tickets[index], orderPath)
		if code != sshSlotBusy {
			t.Fatalf("ticket %s exited %d, want %d: %s", tickets[index], code, sshSlotBusy, output)
		}
	}
	for _, index := range []int{0, 1, 2} {
		code, output := runQueueGate(t, home, tickets[index], orderPath)
		if code != 0 {
			t.Fatalf("ticket %s exited %d, want success: %s", tickets[index], code, output)
		}
	}
	order, err := os.ReadFile(orderPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(order), strings.Join(tickets, "\n")+"\n"; got != want {
		t.Fatalf("run order = %q, want arrival order %q", got, want)
	}
}

// TestSSHHookGateRemovesDeadTickets checks expiry by heartbeat age.
func TestSSHHookGateRemovesDeadTickets(t *testing.T) {
	requireGateHostShell(t)
	home := t.TempDir()
	queue := filepath.Join(home, "rjc-hook", "queue")
	if err := os.MkdirAll(queue, 0o755); err != nil {
		t.Fatal(err)
	}
	dead := filepath.Join(queue, "00000000000000000001-dead")
	live := filepath.Join(queue, "00000000000000000002-live")
	if err := os.WriteFile(dead, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3*slotPollInterval - time.Minute)
	if err := os.Chtimes(dead, old, old); err != nil {
		t.Fatal(err)
	}
	createQueueTicket(t, live)
	code, output := runQueueGate(t, home, "live", filepath.Join(home, "order"))
	if code != 0 {
		t.Fatalf("live ticket behind a dead one exited %d, want success: %s", code, output)
	}
	if _, err := os.Stat(dead); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dead ticket still exists, stat error = %v", err)
	}
}

func TestSSHHookGateRefreshesOwnStaleTicket(t *testing.T) {
	requireGateHostShell(t)
	home := t.TempDir()
	path := filepath.Join(home, "rjc-hook", "queue", "00000000000000000001-first")
	createQueueTicket(t, path)
	old := time.Now().Add(-4 * slotPollInterval)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	createQueueTicket(t, filepath.Join(filepath.Dir(path), "00000000000000000002-second"))
	code, output := runQueueGate(t, home, "first", filepath.Join(home, "order"))
	if code != 0 {
		t.Fatalf("stale caller exited %d: %s", code, output)
	}
}

func TestSSHHookSlotKeepsTicketOnMissingBundle(t *testing.T) {
	requireGateHostShell(t)
	home := t.TempDir()
	queue := filepath.Join(home, "rjc-hook", "queue")
	first := filepath.Join(queue, "00000000000000000001-first")
	createQueueTicket(t, first)
	createQueueTicket(t, filepath.Join(queue, "00000000000000000002-second"))
	d := t.TempDir()
	cmd := exec.Command("sh", "-c", sshHookRun, "run", d, home, filepath.Join(home, "missing.git"), "base", "head", "0", "1", sshHookSlot, ":", ":", "hk", "hk.pkl", "30s")
	cmd.Env = append(os.Environ(), "HOME="+home, "RJC_HOOK_TICKET=first")
	out, err := cmd.CombinedOutput()
	if exit, ok := errors.AsType[*exec.ExitError](err); !ok || exit.ExitCode() != 75 {
		t.Fatalf("expected 75: %v %s", err, out)
	}
	if _, err := os.Stat(first); err != nil {
		t.Fatalf("ticket lost on retry: %v", err)
	}
	code, output := runQueueGate(t, home, "first", filepath.Join(home, "order"))
	if code != 0 {
		t.Fatalf("retry lost queue position: %d %s", code, output)
	}
}

// A new client without a matching ticket cannot bypass a live queue.
func TestSSHHookGateRefusesTicketlessRunWithQueue(t *testing.T) {
	requireGateHostShell(t)
	home := t.TempDir()
	ticket := filepath.Join(home, "rjc-hook", "queue", "00000000000000000001-live")
	createQueueTicket(t, ticket)
	code, output := runQueueGate(t, home, "", filepath.Join(home, "order"))
	if code != sshSlotBusy {
		t.Fatalf("ticketless run exited %d, want %d while the queue is non-empty: %s", code, sshSlotBusy, output)
	}
}

func TestSSHProbeAssignsHostArrivalOrder(t *testing.T) {
	requireGateHostShell(t)
	home := t.TempDir()
	holdQueueFile(t, filepath.Join(home, "rjc-hook", "slots", "s1"))
	ids := []string{"z-client", "a-client", "m-client"}
	for i, id := range ids {
		cmd := exec.Command("sh", "-c", sshHostProbeScript, "probe", "1", id)
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		status, err := parseHostStatus(string(out))
		if err != nil || status.queuePosition != i+1 || status.queueTotal != i+1 {
			t.Fatalf("probe %s: %+v, %v", id, status, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(home, "rjc-hook", "queue"))
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range ids {
		if !strings.HasSuffix(entries[i+1].Name(), "-"+id) {
			t.Fatalf("ticket order: %v", entries)
		}
	}
}

func TestSSHHookFailedUploadStopsHeartbeat(t *testing.T) {
	requireGateHostShell(t)
	home := t.TempDir()
	ticket := filepath.Join(home, "rjc-hook", "queue", "00000000000000000001-first")
	createQueueTicket(t, ticket)
	d := t.TempDir()
	if err := os.Mkdir(filepath.Join(d, "in.bundle"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "sh", "-c", sshHookRun, "run", d, home, home, "base", "head", "0", "1", ":", ":", ":", "hk", "hk.pkl", "30s")
	cmd.Env = append(os.Environ(), "HOME="+home, "RJC_HOOK_TICKET=first")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, stdout); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat retained output pipe after failed upload")
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("upload should fail")
	}
	old := time.Now().Add(-4 * slotPollInterval)
	if err := os.Chtimes(ticket, old, old); err != nil {
		t.Fatal(err)
	}
	code, gateOutput := runQueueGate(t, home, "", filepath.Join(home, "order"))
	if code != 0 {
		t.Fatalf("expired ticket blocked gate: %d %s", code, gateOutput)
	}
	if _, err := os.Stat(ticket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ticket not expired: %v", err)
	}
}

func TestSSHHookProbeDuringBundleCheck(t *testing.T) {
	requireGateHostShell(t)
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFakeTool(t, filepath.Join(bin, "git"), "#!/bin/sh\necho ready > \"$RJC_READY\"\nread x < \"$RJC_RELEASE\"\nexit 1\n")
	ready := filepath.Join(home, "ready")
	release := filepath.Join(home, "release")
	if err := exec.Command("mkfifo", release).Run(); err != nil {
		t.Fatal(err)
	}
	d := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "sh", "-c", sshHookRun, "run", d, home, home, "base", "head", "0", "1", sshHookSlot, ":", ":", "hk", "hk.pkl", "30s")
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "RJC_READY="+ready, "RJC_RELEASE="+release)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cmd.Wait(); err != nil {
			if _, ok := errors.AsType[*exec.ExitError](err); !ok {
				t.Error(err)
			}
		}
	})
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("bundle check did not start")
		case <-time.After(10 * time.Millisecond):
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	probe := exec.CommandContext(ctx, "sh", "-c", sshHostProbeScript, "probe", "1", "")
	probe.Env = append(os.Environ(), "HOME="+home)
	err := probe.Run()
	fifo, openErr := os.OpenFile(release, os.O_WRONLY, 0)
	if openErr != nil {
		t.Fatal(openErr)
	}
	if _, writeErr := fifo.WriteString("release\n"); writeErr != nil {
		t.Fatal(writeErr)
	}
	if closeErr := fifo.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil {
		t.Fatalf("probe blocked during bundle check: %v", err)
	}
}

func TestSSHHookSlotKeepsPlaceWhenBundleCheckOutlastsExpiry(t *testing.T) {
	requireGateHostShell(t)
	home := t.TempDir()
	queue := filepath.Join(home, "rjc-hook", "queue")
	first := filepath.Join(queue, "00000000000000000001-first")
	createQueueTicket(t, first)
	createQueueTicket(t, filepath.Join(queue, "00000000000000000002-second"))
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFakeTool(t, filepath.Join(bin, "git"), "#!/bin/sh\nsleep 4\nexit 1\n")
	script := strings.ReplaceAll(sshHookRun, "sleep 30", "sleep 1")
	script = strings.ReplaceAll(script, "90 seconds ago", "3 seconds ago")
	d := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "sh", "-c", script, "run", d, home, home, "base", "head", "0", "1", sshHookSlot, ":", ":", "hk", "hk.pkl", "30s")
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "RJC_HOOK_TICKET=first")
	out, err := cmd.CombinedOutput()
	if exit, ok := errors.AsType[*exec.ExitError](err); !ok || exit.ExitCode() != 75 {
		t.Fatalf("expected 75: %v %s", err, out)
	}
	info, err := os.Stat(first)
	if err != nil {
		t.Fatalf("ticket missing: %v", err)
	}
	if time.Since(info.ModTime()) > 3*time.Second {
		t.Fatal("ticket heartbeat expired during bundle check")
	}
	code, output := runQueueGate(t, home, "first", filepath.Join(home, "order"))
	if code != 0 {
		t.Fatalf("retry lost position: %d %s", code, output)
	}
}
