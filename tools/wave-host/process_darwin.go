package main

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func Identify(pid int) (Identity, error) {
	return identifyDarwin(pid, func(pid int) (int, int64, int64, error) {
		proc, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
		if err != nil {
			return 0, 0, 0, err
		}
		return int(proc.Proc.P_pid), proc.Proc.P_starttime.Sec, int64(proc.Proc.P_starttime.Usec), nil
	})
}

func Children(pid int) ([]int, error) {
	if pid <= 0 {
		return nil, errors.New("invalid pid")
	}
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}
	children := []int{}
	for _, proc := range procs {
		if int(proc.Eproc.Ppid) == pid {
			children = append(children, int(proc.Proc.P_pid))
		}
	}
	return children, nil
}
