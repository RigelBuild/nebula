//go:build !linux

package command

import "sync/atomic"

// killGroup SIGKILLs the process group only while the leader is unreaped. Without Waitid, the
// waiter sets reaped before it returns, and the check reads that flag without consuming it.
func killGroup(pid int, reaped *atomic.Bool) error {
	if reaped.Load() {
		return nil
	}
	return signalGroup(pid)
}
