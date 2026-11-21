package proc

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// readPinned reads a regular file opened beneath a directory fd without following symlinks.
func readPinned(dirFD int, name string) ([]byte, error) {
	fd, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close() //nolint:errcheck // The read completed; a read-only close cannot lose data.
	return io.ReadAll(file)
}

// CopyProcMemory copies [start, end) of a process read-only into outputPath. The starttime
// check after pinning the pid rejects a reused pid; the mem fd stays bound to the pinned task.
func CopyProcMemory(procRoot, pid string, start, end uint64, outputPath, identity string) (err error) {
	separator := strings.Index(identity, ":")
	if separator < 1 || identity[:separator] != pid || !IsDigits(identity[separator+1:]) || end <= start {
		return errors.New("invalid proc memory range or server identity")
	}
	expectedStart := identity[separator+1:]
	procDirFD, err := unix.Open(procRoot+"/"+pid, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, unix.Close(procDirFD)) }()
	stat, err := readPinned(procDirFD, "stat")
	if err != nil {
		return err
	}
	if StatStartTime(string(stat)) != expectedStart {
		return errors.New("server identity changed before proc memory open")
	}
	memFD, err := unix.Openat(procDirFD, "mem", unix.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	memFile := os.NewFile(uintptr(memFD), "mem")
	defer func() { err = errors.Join(err, memFile.Close()) }()
	// Raw heap bytes stay root-only; only the reduced strings are group-readable.
	out, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // G304: outputPath is a mapping-N.raw inside the watchdog's own capture dir, never external input.
	if err != nil {
		return err
	}
	copyErr := CopyRanges(memFile, out, start, end)
	return errors.Join(copyErr, out.Close())
}

// openDirectoryNoFollow opens an absolute directory without following a symlink at any component.
func openDirectoryNoFollow(path string) (int, error) {
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return -1, err
	}
	for part := range strings.SplitSeq(path, "/") {
		if part == "" {
			continue
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if closeErr := unix.Close(fd); closeErr != nil {
			return -1, errors.Join(err, closeErr)
		}
		if err != nil {
			return -1, err
		}
		fd = next
	}
	return fd, nil
}

// ReadLogTail reads the last byteCount bytes of a regular file in directory, following no symlink.
func ReadLogTail(directory, name string, byteCount int64) ([]byte, error) {
	dirFD, err := openDirectoryNoFollow(directory)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if closeErr := unix.Close(dirFD); closeErr != nil {
		return nil, errors.Join(err, closeErr)
	}
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close() //nolint:errcheck // Read-only tail read; the bytes are already copied out.
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, errors.New("not a regular file")
	}
	count := min(byteCount, stat.Size)
	buf := make([]byte, count)
	n, err := file.ReadAt(buf, stat.Size-count)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("reading log tail: %w", err)
	}
	return buf[:n], nil
}
