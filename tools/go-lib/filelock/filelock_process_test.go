package filelock

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestWithWaitsForLockAndRecoversAfterSIGKILL(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "session.lock")
	holder := exec.Command(os.Args[0], "-test.run=^TestFlockProcessHelper$")
	holder.Env = append(os.Environ(), "FILELOCK_HELPER=1", "FILELOCK_PATH="+lockPath)
	holderStdin, err := holder.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	holderStdout, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	holder.Stderr = io.Discard
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}

	holderWaited := false
	reapHolder := func() error {
		if holderWaited {
			return nil
		}
		holderWaited = true
		return killLockHolder(holder)
	}
	t.Cleanup(func() { cleanupLockHolder(t, &holderWaited, reapHolder, holderStdin, holderStdout) })

	marker, readErr := bufio.NewReader(holderStdout).ReadString('\n')
	if readErr != nil || marker != "locked\n" {
		t.Fatalf("lock holder handshake = %q, %v; want locked marker", marker, readErr)
	}
	assertFlockContended(t, lockPath)
	assertHeldLockBlocksWork(t, lockPath)

	if err := reapHolder(); err != nil {
		t.Fatalf("killing and joining lock holder pid %d: %v", holder.Process.Pid, err)
	}

	recoveredWorkRan := false
	if err := With(t.Context(), lockPath, func() error {
		recoveredWorkRan = true
		return nil
	}); err != nil {
		t.Fatalf("With() after killing lock holder: %v", err)
	}
	if !recoveredWorkRan {
		t.Fatal("With() did not enter work after the holder exited")
	}
}

func TestFlockProcessHelper(t *testing.T) {
	if os.Getenv("FILELOCK_HELPER") != "1" {
		return
	}
	file, err := os.OpenFile(os.Getenv("FILELOCK_PATH"), os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(os.Stdout, "locked"); err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
}

func assertFlockContended(t *testing.T, lockPath string) {
	t.Helper()
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	}()
	err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
		t.Fatalf("nonblocking flock error = %v, want lock contention", err)
	}
}

func assertHeldLockBlocksWork(t *testing.T, lockPath string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	workRan := false
	err := With(ctx, lockPath, func() error {
		workRan = true
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("With() with held lock error = %v, want context deadline exceeded", err)
	}
	if workRan {
		t.Fatal("With() ran work while another process held the lock")
	}
}

func cleanupLockHolder(t *testing.T, waited *bool, reap func() error, stdin io.Closer, stdout io.Closer) {
	t.Helper()
	if !*waited {
		if err := reap(); err != nil {
			t.Errorf("reaping lock holder: %v", err)
		}
	}
	if err := stdin.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		t.Errorf("closing lock holder stdin: %v", err)
	}
	if err := stdout.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		t.Errorf("closing lock holder stdout: %v", err)
	}
}

func killLockHolder(holder *exec.Cmd) error {
	killErr := holder.Process.Kill()
	waitErr := holder.Wait()
	if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
		return errors.Join(killErr, waitErr)
	}
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		return errors.Join(errors.New("lock holder did not exit from SIGKILL"), waitErr)
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		return errors.New("lock holder did not exit from SIGKILL")
	}
	return nil
}
