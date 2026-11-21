//go:build !linux

package proc

import "errors"

var errLinuxOnly = errors.New("proc memory and pinned log reads require Linux")

// CopyProcMemory is Linux-only; other platforms cannot pin /proc/<pid>/mem.
func CopyProcMemory(_, _ string, _, _ uint64, _, _ string) error {
	return errLinuxOnly
}

// ReadLogTail is Linux-only; other platforms cannot pin the log directory without following symlinks.
func ReadLogTail(_, _ string, _ int64) ([]byte, error) {
	return nil, errLinuxOnly
}
