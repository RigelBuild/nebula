package fence

import (
	"os"
	"syscall"
)

// changeTime returns the inode change time in nanoseconds. OMP compares ctime
// as well as size and mtime, so a lock rewritten with its mtime restored still
// counts as changed. os.Stat always reports *syscall.Stat_t on this platform.
func changeTime(info os.FileInfo) int64 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return stat.Ctimespec.Nano()
}
