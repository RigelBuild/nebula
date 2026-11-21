//go:build linux

package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec" //nolint:depguard // Image setup must invoke mkfs.ext4 directly without a shell.
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func fsTypeName(t int64) string {
	if uint32(t) == unix.BTRFS_SUPER_MAGIC { //nolint:gosec // Filesystem magic is 32-bit.
		return fsBtrfs
	}
	return fmt.Sprintf("0x%x", uint32(t)) //nolint:gosec // Filesystem magic is 32-bit.
}

// fsNoCOWFlag is FS_NOCOW_FL from linux/fs.h. x/sys does not export it.
const fsNoCOWFlag = 0x00800000

// realFS implements imageFS against the kernel and the mkfs.ext4 binary.
type realFS struct {
	mkfsPath string
	// mountinfo is the path of the mount table; /proc/self/mountinfo in production.
	mountinfo string
}

func (r realFS) Exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func (realFS) FilesystemType(path string) (string, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return "", err
	}
	return fsTypeName(st.Type), nil
}

func (realFS) NoCOW(path string) (nocow bool, resultErr error) {
	file, err := os.Open(path) //nolint:gosec // Path is the operator-configured image file.
	if err != nil {
		return false, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, file.Close())
	}()
	flags, err := getFlags(file.Fd())
	if err != nil {
		return false, err
	}
	return flags&fsNoCOWFlag != 0, nil
}

func (realFS) CreateEmpty(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // The temporary path is derived from the operator-configured image file.
	if err != nil {
		return err
	}
	return file.Close()
}

func (realFS) SetNoCOW(path string) (resultErr error) {
	file, err := os.OpenFile(path, os.O_RDONLY, 0) //nolint:gosec // Path is the operator-configured image file.
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, file.Close())
	}()
	flags, err := getFlags(file.Fd())
	if err != nil {
		return translateNotSupported(err)
	}
	if err := setFlags(file.Fd(), flags|fsNoCOWFlag); err != nil {
		return translateNotSupported(err)
	}
	return nil
}

// Truncate sets the apparent size of path. It allocates no blocks, so the
// image takes disk space only as the filesystem writes to it.
func (realFS) Truncate(path string, size int64) (resultErr error) {
	file, err := os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec // Path is the operator-configured image file.
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, file.Close())
	}()
	return file.Truncate(size)
}

func (r realFS) Mkfs(path string) error {
	out, err := exec.Command(r.mkfsPath, "-F", path).CombinedOutput() //nolint:gosec // Command and args come from operator configuration; argv is passed without a shell.
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (realFS) Rename(from, to string) error {
	return os.Rename(from, to)
}

// Sync flushes path to stable storage. A read-only handle is enough on Linux
// for both a regular file and a directory.
func (realFS) Sync(path string) (resultErr error) {
	file, err := os.Open(path) //nolint:gosec // Path is the configured image or its parent directory.
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, file.Close())
	}()
	return file.Sync()
}

// Mounted reports whether mount is listed as a mount point in the mount table.
func (r realFS) Mounted(mount string) (mounted bool, resultErr error) {
	file, err := os.Open(r.mountinfo)
	if err != nil {
		return false, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, file.Close())
	}()
	want := filepath.Clean(mount)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		// Field 5 is the mount point, octal-escaped by the kernel; the configured
		// mount path has no whitespace or backslash, so a raw compare is exact.
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 5 && fields[4] == want {
			return true, nil
		}
	}
	return false, scanner.Err()
}

// getFlags reads the inode flags through FS_IOC_GETFLAGS.
func getFlags(fd uintptr) (uint32, error) {
	var flags uint32
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, unix.FS_IOC_GETFLAGS, uintptr(unsafe.Pointer(&flags))) //nolint:gosec // ioctl requires this pointer to the local flags word.
	if errno != 0 {
		return 0, errno
	}
	return flags, nil
}

// setFlags writes the inode flags through FS_IOC_SETFLAGS.
func setFlags(fd uintptr, flags uint32) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, unix.FS_IOC_SETFLAGS, uintptr(unsafe.Pointer(&flags))) //nolint:gosec // ioctl requires this pointer to the local flags word.
	if errno != 0 {
		return errno
	}
	return nil
}

// translateNotSupported maps the kernel's EOPNOTSUPP to the seam's sentinel,
// so prepare can ignore only that case.
func translateNotSupported(err error) error {
	if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOTTY) {
		return errOperationNotSupported
	}
	return err
}
