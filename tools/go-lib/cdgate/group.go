package cdgate

import (
	"errors"
	"strconv"
	"strings"
	"syscall"
)

// Kill sends sig to pid. SignalGroup passes a negative pid to address a
// process group. Tests inject one; production uses syscall.Kill.
type Kill func(pid int, sig syscall.Signal) error

// ListProcesses returns the `ps -A -o pgid=,stat=` table: one "pgid stat" row
// per process. Tests inject one; production runs ps.
type ListProcesses func() (string, error)

// hasLiveGroupMember reports whether a non-zombie process belongs to group pid.
// A zombie (state starting with Z) is already dead and does not count.
func hasLiveGroupMember(pid int, processes string) bool {
	for line := range strings.SplitSeq(processes, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		group, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		if group == pid && !strings.HasPrefix(fields[1], "Z") {
			return true
		}
	}
	return false
}

// SignalGroup sends sig to the process group pid. It reports whether any
// member could still be alive. ESRCH means the group is gone, so it returns
// false. EPERM means the group exists but the signal is refused, so it
// returns true only if a live member remains. Any other error is returned.
func SignalGroup(pid int, sig syscall.Signal, kill Kill, list ListProcesses) (bool, error) {
	err := kill(-pid, sig)
	if err == nil {
		return true, nil
	}
	switch {
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	case errors.Is(err, syscall.EPERM):
		processes, listErr := list()
		if listErr != nil {
			return false, listErr
		}
		return hasLiveGroupMember(pid, processes), nil
	default:
		return false, err
	}
}
