package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/fence"
)

func TestRootCommandRegistersFence(t *testing.T) {
	command, _, err := newRootCmd().Find([]string{"fence"})
	if err != nil {
		t.Fatal(err)
	}
	if command.Name() != "fence" {
		t.Fatalf("command = %q, want fence", command.Name())
	}
	if command.Flags().Lookup("agent-dir") == nil {
		t.Fatal("fence command is missing --agent-dir")
	}
	if command.Flags().Lookup("wait") == nil {
		t.Fatal("fence command is missing --wait")
	}
}

func TestFenceCommandReturnsExitCodeThreeWhenOMPUnavailable(t *testing.T) {
	// The fence command must surface Run's error instead of swallowing it. With
	// OMP absent, Run fails before any lock is taken, so the command must return
	// an error that maps to exit 3. The cancel contract is covered in the fence
	// package, where Run is driven directly.
	t.Setenv("PATH", t.TempDir())
	command := newFenceCmd()
	command.SetArgs([]string{"--agent-dir", t.TempDir()})
	err := command.ExecuteContext(t.Context())
	if err == nil {
		t.Fatal("fence returned nil with OMP unavailable, want non-zero exit")
	}
	if exitCode(err) != 3 {
		t.Fatalf("exitCode(%v) = %d, want 3 for unavailable OMP", err, exitCode(err))
	}
}

func TestExitCode(t *testing.T) {
	externalErr := exec.Command("/bin/sh", "-c", "exit 3").Run()
	if externalErr == nil {
		t.Fatal("child process unexpectedly succeeded")
	}
	if _, ok := errors.AsType[*exec.ExitError](externalErr); !ok {
		t.Fatalf("child error = %T, want *exec.ExitError", externalErr)
	}

	tests := []struct {
		name string
		err  error
		want int
	}{
		{
			name: "unsupported OMP version",
			err:  fmt.Errorf("fence: %w", &fence.ExitError{Code: 3, Message: "unsupported omp version"}),
			want: 3,
		},
		{
			name: "live PID lock",
			err:  fmt.Errorf("fence: %w", &fence.ExitError{Code: 4, Message: "live lock"}),
			want: 4,
		},
		{
			name: "wrapped external process error",
			err:  fmt.Errorf("restic backup: %w", externalErr),
			want: 1,
		},
		{
			name: "ordinary error",
			err:  errors.New("failed to fence agent directory"),
			want: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := exitCode(test.err); got != test.want {
				t.Fatalf("exitCode(%v) = %d, want %d", test.err, got, test.want)
			}
		})
	}
}

func TestFenceCommandCancelDuringAcquireExitsNonZero(t *testing.T) {
	installFakeOMP(t)
	root := t.TempDir()
	heldDir := filepath.Join(root, "held")
	liveDir := filepath.Join(root, "live")
	writeLiveLock(t, liveDir)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	command := newFenceCmd()
	command.SetArgs([]string{"--agent-dir", heldDir, "--agent-dir", liveDir, "--wait", "1h"})
	done := make(chan error, 1)
	go func() {
		done <- command.ExecuteContext(ctx)
	}()
	// held is free, so the fence takes it. live is held by a live PID, so the
	// fence waits on it until the cancel below.
	waitForGCLock(t, filepath.Join(heldDir, "gc.lock"))
	cancel()
	err := awaitFence(t, done)
	if err == nil {
		t.Fatal("fence returned nil after cancel during acquire, want non-zero exit")
	}
	if exitCode(err) == 0 {
		t.Fatalf("exitCode(%v) = 0, want non-zero", err)
	}
}

func TestFenceCommandCancelDuringHoldExitsZero(t *testing.T) {
	installFakeOMP(t)
	root := t.TempDir()
	firstDir := filepath.Join(root, "first")
	secondDir := filepath.Join(root, "second")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	command := newFenceCmd()
	command.SetArgs([]string{"--agent-dir", firstDir, "--agent-dir", secondDir, "--wait", "1h"})
	done := make(chan error, 1)
	go func() {
		done <- command.ExecuteContext(ctx)
	}()
	// Wait for both locks before cancelling, so the cancel lands in the hold phase.
	waitForGCLock(t, filepath.Join(firstDir, "gc.lock"))
	waitForGCLock(t, filepath.Join(secondDir, "gc.lock"))
	cancel()
	if err := awaitFence(t, done); err != nil {
		t.Fatalf("cancel during hold returned %v, want nil (exit 0)", err)
	}
}

func installFakeOMP(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	fakeOMP := filepath.Join(binDir, "omp")
	script := "#!/bin/sh\nprintf '%s\\n' '" + fence.OMPVersion + "'\n"
	if err := os.WriteFile(fakeOMP, []byte(script), 0o600); err != nil {
		t.Fatal("write fake OMP")
	}
	if err := os.Chmod(fakeOMP, 0o700); err != nil {
		t.Fatal("make fake OMP executable")
	}
	t.Setenv("PATH", binDir)
}

// writeLiveLock writes agentDir's gc.lock with this test process's PID and a
// fresh timestamp, so the fence sees a lock held by a live process.
func writeLiveLock(t *testing.T, agentDir string) {
	t.Helper()
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal("create live agent directory fixture")
	}
	contents := fmt.Sprintf("%d\n%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))
	if err := os.WriteFile(filepath.Join(agentDir, "gc.lock"), []byte(contents), 0o600); err != nil {
		t.Fatal("write live gc lock fixture")
	}
}

// waitForGCLock polls until path exists, so a cancel lands after the fence has
// taken that lock.
func waitForGCLock(t *testing.T, path string) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case <-timer.C:
			t.Fatalf("fence did not take %s", path)
		case <-ticker.C:
		}
	}
}

// awaitFence waits for the fence command to return after its context is cancelled.
func awaitFence(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("fence did not return after cancel")
		return nil
	}
}
