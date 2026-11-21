package command

import (
	"errors"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/unix"
)

// killGroup SIGKILLs the process group only while the leader is unreaped, so its pgid cannot
// have been reused. Waitid with WNOWAIT inspects the leader without reaping it. The reaped flag
// is checked first so a leader already collected by the waiter is never signalled.
func killGroup(pid int, reaped *atomic.Bool) error {
	if reaped.Load() {
		return nil
	}
	var info unix.Siginfo
	err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil)
	switch {
	case err == nil:
		return signalGroup(pid)
	case errors.Is(err, syscall.ECHILD):
		return nil
	default:
		return err
	}
}
