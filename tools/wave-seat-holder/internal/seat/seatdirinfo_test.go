package seat

import (
	"io/fs"
	"os"
	"syscall"
	"time"
)

// fakeUID is the owner the fake seat directory reports. It is this process's uid, so ownership checks pass.
var fakeUID = os.Getuid()

// fakeDirInfo is a directory with a fixed mode that reports its owner through a real *syscall.Stat_t,
// the type checkSeatDirectory asserts on.
type fakeDirInfo struct {
	name string
	mode fs.FileMode
	stat *syscall.Stat_t
}

func (f fakeDirInfo) Name() string       { return f.name }
func (f fakeDirInfo) Size() int64        { return 0 }
func (f fakeDirInfo) Mode() fs.FileMode  { return fs.ModeDir | f.mode }
func (f fakeDirInfo) ModTime() time.Time { return time.Time{} }
func (f fakeDirInfo) IsDir() bool        { return true }
func (f fakeDirInfo) Sys() any           { return f.stat }

// ownedSeatDirInfo is the fake Lstat result for a seat directory the holder already owns at 0700.
func ownedSeatDirInfo() fs.FileInfo {
	return fakeDirInfo{name: "seats", mode: 0o700, stat: &syscall.Stat_t{Uid: uint32(fakeUID)}}
}
