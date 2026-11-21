//go:build linux

package proc

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/RigelBuild/nebula/tools/zellij-rss-watchdog/internal/command"
)

// procGone reports whether /proc/<pid> has vanished, the observable death signal.
func procGone(pid int) bool {
	_, err := os.Stat("/proc/" + strconv.Itoa(pid))
	return os.IsNotExist(err)
}

// readPIDFile waits until the child writes a pid to path, then returns it.
func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	var pid int
	requireUntil(t, 10*time.Second, "pid file "+path, func() bool {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		value, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			return false
		}
		pid = value
		return true
	})
	return pid
}

// killPID kills a test-owned pid we started; ESRCH means it already exited, which is fine.
func killPID(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}

// makeFIFO creates a named pipe at path for the non-regular-file case.
func makeFIFO(path string) error {
	return syscall.Mkfifo(path, 0o600)
}

func TestTimeoutKillsGroupMemberThatIgnoresSIGTERMEvenWhenLeaderWouldExit(t *testing.T) {
	directory := t.TempDir()
	pidFile := filepath.Join(directory, "member.pid")
	_, err := command.Text(context.Background(), "bash", []string{
		"-c", `(trap "" TERM; echo $BASHPID > "$1"; exec sleep 30) & sleep 30`, "watchdog", pidFile,
	}, 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "command timed out after 0.3s") {
		t.Fatalf("err = %v, want timeout after 0.3s", err)
	}
	member := readPIDFile(t, pidFile)
	requireUntil(t, 5*time.Second, "group member to exit", func() bool { return procGone(member) })
}

func TestTimeoutSettlesWhenDescendantOutsideGroupKeepsPipesOpen(t *testing.T) {
	// setsid puts the grandchild in another session, out of reach of the group signal.
	directory := t.TempDir()
	pidFile := filepath.Join(directory, "escaped.pid")
	started := time.Now()
	_, err := command.Text(context.Background(), "bash", []string{
		"-c", `setsid bash -c "echo \$\$ > \"$1\"; exec sleep 8" _ "$1" & wait`, "watchdog", pidFile,
	}, 200*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "command timed out after 0.2s") {
		t.Fatalf("err = %v, want timeout after 0.2s", err)
	}
	if elapsed := time.Since(started); elapsed >= 5*time.Second {
		t.Fatalf("settled after %s, want under 5s", elapsed)
	}
	t.Cleanup(func() {
		data, readErr := os.ReadFile(pidFile)
		if readErr != nil {
			return
		}
		if escaped, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && escaped > 1 {
			if killErr := killPID(escaped); killErr != nil {
				t.Errorf("cleanup of escaped test process %d: %v", escaped, killErr)
			}
		}
	})
}

func TestTimedOutCommandWholeProcessGroupIsKilled(t *testing.T) {
	directory := t.TempDir()
	pidFile := filepath.Join(directory, "grandchild.pid")
	_, err := command.Text(context.Background(), "bash", []string{
		"-c", `sleep 30 & echo $! > "$1"; wait`, "watchdog", pidFile,
	}, 200*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "command timed out after 0.2s") {
		t.Fatalf("err = %v, want timeout after 0.2s", err)
	}
	grandchild := readPIDFile(t, pidFile)
	requireUntil(t, 5*time.Second, "grandchild to exit", func() bool { return procGone(grandchild) })
}

func TestCopyProcMemoryCopiesLiveRangeAndRefusesReusedPid(t *testing.T) {
	directory := t.TempDir()
	// A heap-backed marker keeps one address for the copy window; KeepAlive pins it.
	marker := []byte("watchdog-proc-memory-marker")
	address := uint64(uintptr(unsafe.Pointer(&marker[0])))
	defer runtime.KeepAlive(marker)

	stat, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Fatal(err)
	}
	starttime := StatStartTime(string(stat))
	pid := strconv.Itoa(os.Getpid())
	output := filepath.Join(directory, "copy.bin")
	must(t, CopyProcMemory("/proc", pid, address, address+uint64(len(marker)), output, pid+":"+starttime))
	got, err := os.ReadFile(output)
	must(t, err)
	if string(got) != string(marker) {
		t.Fatalf("copied %q, want %q", got, marker)
	}
	stale := filepath.Join(directory, "stale.bin")
	err = CopyProcMemory("/proc", pid, address, address+1, stale, pid+":1")
	if err == nil || !strings.Contains(err.Error(), "server identity changed before proc memory open") {
		t.Fatalf("err = %v, want identity-change refusal", err)
	}
	if _, statErr := os.Stat(stale); !os.IsNotExist(statErr) {
		t.Fatalf("stale output exists after refusal: %v", statErr)
	}
}

func TestReadLogTailReadsTailAndRefusesSymlinksAndNonRegularFiles(t *testing.T) {
	directory := t.TempDir()
	must(t, os.WriteFile(filepath.Join(directory, "zellij.log"), []byte("0123456789"), 0o600))
	must(t, os.WriteFile(filepath.Join(directory, "secret"), []byte("do-not-read"), 0o600))
	must(t, os.Symlink(filepath.Join(directory, "secret"), filepath.Join(directory, "link.log")))
	must(t, makeFIFO(filepath.Join(directory, "fifo.log")))

	tail, err := ReadLogTail(directory, "zellij.log", 4)
	must(t, err)
	if string(tail) != "6789" {
		t.Fatalf("tail = %q, want 6789", tail)
	}
	if _, err := ReadLogTail(directory, "link.log", 4); err == nil {
		t.Fatal("symlink was followed")
	}
	if _, err := ReadLogTail(directory, "fifo.log", 4); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("fifo err = %v, want not a regular file", err)
	}
	must(t, os.Symlink(directory, filepath.Join(directory, "redirect")))
	if _, err := ReadLogTail(filepath.Join(directory, "redirect"), "zellij.log", 4); err == nil {
		t.Fatal("symlinked directory component was followed")
	}
}
