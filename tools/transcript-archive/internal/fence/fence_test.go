package fence

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRunFencesOMPBlobGcAndBreaksDeadLock(t *testing.T) {
	ompPath := requireOMP(t)
	t.Setenv("PATH", filepath.Dir(ompPath)+string(os.PathListSeparator)+os.Getenv("PATH"))

	agentDir := t.TempDir()
	blobPath := filepath.Join(agentDir, "blobs", strings.Repeat("a", 64))
	writeTestFile(t, blobPath, []byte("unchanged blob"))
	oldTime := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(blobPath, oldTime, oldTime); err != nil {
		t.Fatal("age blob fixture")
	}
	lockPath := filepath.Join(agentDir, "gc.lock")
	if err := os.WriteFile(lockPath, []byte("2147483647\n2026-01-01T00:00:00.000Z\n"), 0o600); err != nil {
		t.Fatal("write dead lock fixture")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runDone := make(chan error, 1)
	go func() {
		runDone <- Run(ctx, Options{AgentDirs: []string{agentDir}})
	}()
	waitForLockPID(t, lockPath)

	lockText, err := os.ReadFile(lockPath)
	if err != nil || !strings.HasPrefix(string(lockText), strconv.Itoa(os.Getpid())+"\n") {
		cancel()
		t.Fatal("fence did not replace the dead-PID lock with its own OMP-format lock")
	}
	command := exec.CommandContext(t.Context(), "omp", "gc", "--agent-dir", agentDir, "--blobs", "--apply")
	output, commandErr := command.CombinedOutput()
	if commandErr == nil || !strings.Contains(string(output), "GC already running") {
		cancel()
		t.Fatal("omp gc did not fail because the fence held its gc lock")
	}
	blob, err := os.ReadFile(blobPath)
	if err != nil || string(blob) != "unchanged blob" {
		cancel()
		t.Fatal("omp gc changed the fixture blob while the fence was active")
	}

	cancel()
	if err := <-runDone; err != nil {
		t.Fatalf("release fence: %v", err)
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("fence left its gc lock behind")
	}

	// Control: with the fence released, the same apply must delete the aged blob.
	// If it does not, the blob-unchanged check above could not have failed.
	control := exec.CommandContext(t.Context(), "omp", "gc", "--agent-dir", agentDir, "--blobs", "--apply")
	if output, err := control.CombinedOutput(); err != nil {
		t.Fatalf("control omp gc --apply after release: %v\n%s", err, output)
	}
	if _, err := os.Stat(blobPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("control omp gc --apply did not delete the aged blob after the fence was released")
	}
}

func TestRunReturnsExitCodeFourForLiveLock(t *testing.T) {
	installFakeOMP(t)
	agentDir := t.TempDir()
	lockPath := filepath.Join(agentDir, "gc.lock")
	contents := fmt.Sprintf("%d\n%s\n", os.Getpid(), time.Now().UTC().Format("2006-01-02T15:04:05.000Z"))
	if err := os.WriteFile(lockPath, []byte(contents), 0o600); err != nil {
		t.Fatal("write live lock fixture")
	}
	err := Run(t.Context(), Options{AgentDirs: []string{agentDir}, Wait: 25 * time.Millisecond})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 4 {
		t.Fatal("fence did not return exit code 4 for the live lock")
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatal("fence removed a live gc lock")
	}
}

func TestRunReturnsImmediatelyForZeroWait(t *testing.T) {
	installFakeOMP(t)
	agentDir := t.TempDir()
	lockPath := filepath.Join(agentDir, "gc.lock")
	if err := os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))), 0o600); err != nil {
		t.Fatal("write live lock fixture")
	}
	err := Run(t.Context(), Options{AgentDirs: []string{agentDir}, Wait: 0})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 4 {
		t.Fatal("fence did not return exit code 4 for zero wait")
	}
	// requireKeptLock times acquire alone; timing Run here would also count the fake omp fork.
}

func TestRunFencesDefaultAndProfileAgentDirs(t *testing.T) {
	installFakeOMP(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	agentDirs := []string{
		filepath.Join(home, ".omp", "agent"),
		filepath.Join(home, ".omp", "profiles", "first", "agent"),
		filepath.Join(home, ".omp", "profiles", "second", "agent"),
		filepath.Join(home, ".omp", "profiles", "linked-profile", "agent"),
	}
	for _, agentDir := range agentDirs[:3] {
		if err := os.MkdirAll(agentDir, 0o700); err != nil {
			t.Fatal("create agent directory fixture")
		}
	}
	profilesDir := filepath.Join(home, ".omp", "profiles")
	if err := os.MkdirAll(filepath.Join(home, "first-profile-target"), 0o700); err != nil {
		t.Fatal("create symlink profile target")
	}
	if err := os.Symlink(filepath.Join(home, "first-profile-target"), filepath.Join(profilesDir, "linked-profile")); err != nil {
		t.Fatal("create symlink profile")
	}
	if err := os.WriteFile(filepath.Join(profilesDir, "not-a-profile"), []byte("x"), 0o600); err != nil {
		t.Fatal("create non-directory profile fixture")
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runDone := make(chan error, 1)
	go func() {
		runDone <- Run(ctx, Options{})
	}()
	lockPaths := make([]string, 0, len(agentDirs))
	for _, agentDir := range agentDirs {
		lockPaths = append(lockPaths, filepath.Join(agentDir, "gc.lock"))
	}
	waitForLocks(t, lockPaths...)
	cancel()
	if err := <-runDone; err != nil {
		t.Fatalf("release profile fences: %v", err)
	}
	for _, lockPath := range lockPaths {
		if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("fence left a profile gc lock behind")
		}
	}
}

func TestRunFencesSymlinkedAgentDirOnce(t *testing.T) {
	installFakeOMP(t)
	root := t.TempDir()
	agentDir := filepath.Join(root, "agent")
	linkDir := filepath.Join(root, "linked-agent")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal("create agent directory fixture")
	}
	if err := os.Symlink(agentDir, linkDir); err != nil {
		t.Fatal("create symlink to agent directory")
	}

	// Wait is zero. Locking the same directory twice would make the second
	// acquire see the first hold's live PID and exit 4 at once.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runDone := make(chan error, 1)
	go func() {
		runDone <- Run(ctx, Options{AgentDirs: []string{agentDir, linkDir}})
	}()
	waitForLocks(t, filepath.Join(agentDir, "gc.lock"))
	cancel()
	if err := <-runDone; err != nil {
		t.Fatalf("release fence over symlinked agent dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(agentDir, "gc.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("fence left its gc lock behind")
	}
}

func TestRunReturnsNilWhenHoldPhaseIsCancelled(t *testing.T) {
	installFakeOMP(t)
	agentDir := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runDone := make(chan error, 1)
	go func() {
		runDone <- Run(ctx, Options{AgentDirs: []string{agentDir}})
	}()
	waitForLocks(t, filepath.Join(agentDir, "gc.lock"))
	cancel()
	if err := <-runDone; err != nil {
		t.Fatalf("cancel during hold returned %v, want nil (exit 0)", err)
	}
	if _, err := os.Stat(filepath.Join(agentDir, "gc.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancel during hold left the gc lock behind")
	}
}

func TestRunReturnsErrorWhenCancelledDuringAcquire(t *testing.T) {
	installFakeOMP(t)
	heldDir := t.TempDir()
	liveDir := t.TempDir()
	livePath := filepath.Join(liveDir, "gc.lock")
	writeTestFile(t, livePath, []byte(fmt.Sprintf("%d\n%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))))
	// Run takes heldDir first, so its gc.lock exists before Run reaches liveDir.
	// liveDir's lock is live, so Run waits in acquire until ctx is cancelled.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runDone := make(chan error, 1)
	go func() {
		runDone <- Run(ctx, Options{AgentDirs: []string{heldDir, liveDir}, Wait: time.Hour})
	}()
	waitForLocks(t, filepath.Join(heldDir, "gc.lock"))
	cancel()
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel during acquire returned %v, want a wrapped context.Canceled (non-zero exit)", err)
	}
	if _, err := os.Stat(filepath.Join(heldDir, "gc.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancel during acquire left the already-taken gc lock behind")
	}
	if _, err := os.Stat(livePath); err != nil {
		t.Fatal("cancel during acquire removed a live gc lock")
	}
}

func TestRunReturnsExitCodeThreeForUnsupportedOMPVersion(t *testing.T) {
	installFakeOMPVersion(t, "omp/1.0.0")
	err := Run(t.Context(), Options{AgentDirs: []string{t.TempDir()}})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Fatal("fence did not return exit code 3 for an unsupported OMP version")
	}
}

func TestRunReturnsExitCodeThreeWhenOMPUnavailable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := Run(t.Context(), Options{AgentDirs: []string{t.TempDir()}})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Fatal("fence did not return exit code 3 when OMP was unavailable")
	}
}

func TestCheckOMPVersionReturnsContextErrorWhenCancelled(t *testing.T) {
	installFakeOMP(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := checkOMPVersion(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("checkOMPVersion with cancelled ctx = %v, want context.Canceled", err)
	}
}

func installFakeOMP(t *testing.T) {
	t.Helper()
	installFakeOMPVersion(t, OMPVersion)
}

func installFakeOMPVersion(t *testing.T, version string) {
	t.Helper()
	binDir := t.TempDir()
	fakeOMP := filepath.Join(binDir, "omp")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' '%s'\n", version)
	writeTestFile(t, fakeOMP, []byte(script))
	if err := os.Chmod(fakeOMP, 0o700); err != nil {
		t.Fatal("make fake OMP executable")
	}
	t.Setenv("PATH", binDir)
}

func requireOMP(t *testing.T) string {
	t.Helper()
	ompPath := os.Getenv("TRANSCRIPT_ARCHIVE_TEST_OMP")
	if ompPath == "" {
		if os.Getenv("TRANSCRIPT_ARCHIVE_REQUIRE_OMP") != "" {
			t.Fatal("OMP integration binary is required but unavailable")
		}
		t.Skip("OMP integration binary is unavailable")
	}
	if !filepath.IsAbs(ompPath) {
		resolvedPath, err := exec.LookPath(ompPath)
		if err != nil {
			t.Fatalf("find OMP integration binary: %v", err)
		}
		ompPath = resolvedPath
	}
	output, err := exec.Command(ompPath, "--version").Output()
	if err != nil {
		t.Fatalf("run OMP integration version: %v", err)
	}
	if strings.TrimSpace(string(output)) != OMPVersion {
		t.Fatalf("OMP version = %q, want %q", strings.TrimSpace(string(output)), OMPVersion)
	}
	t.Setenv("PATH", filepath.Dir(ompPath)+string(os.PathListSeparator)+os.Getenv("PATH"))
	return ompPath
}

func TestShouldBreakLockParsesOMPNumericPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gc.lock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal("create lock stat fixture")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal("stat lock fixture")
	}
	livePID := strconv.Itoa(os.Getpid())
	for _, lockText := range []string{livePID + "\r\n2026-01-01T00:00:00.000Z\r\n", livePID + "abc\n2026-01-01T00:00:00.000Z\n"} {
		snapshot := lockSnapshot{info: info, text: lockText}
		if shouldBreakLock(snapshot) {
			t.Fatalf("shouldBreakLock(%q) broke a lock with a live PID prefix", lockText)
		}
	}
	deadLock := lockSnapshot{info: info, text: "2147483647abc\n2026-01-01T00:00:00.000Z\n"}
	if !shouldBreakLock(deadLock) {
		t.Fatal("shouldBreakLock kept a lock with a dead PID prefix")
	}
}

func writeTestFile(t *testing.T, path string, contents []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal("create fixture directory")
	}
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal("write fixture file")
	}
}

func waitForLockPID(t *testing.T, path string) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		contents, err := os.ReadFile(path)
		if err == nil && strings.HasPrefix(string(contents), strconv.Itoa(os.Getpid())+"\n") {
			return
		}
		select {
		case <-timer.C:
			t.Fatal("fence did not write its PID to the gc lock")
		case <-ticker.C:
		}
	}
}

func waitForLocks(t *testing.T, paths ...string) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		allExist := true
		for _, path := range paths {
			if _, err := os.Stat(path); err != nil {
				allExist = false
				break
			}
		}
		if allExist {
			return
		}
		select {
		case <-timer.C:
			t.Fatal("fence did not acquire all gc locks")
		case <-ticker.C:
		}
	}
}
