package seat

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// currentUID is the holder's uid. It is a variable so the ownership refusal can be tested with another owner.
var currentUID = os.Getuid

// seatDirFD is an open seat directory. Every operation is relative to its file descriptor, so a path swapped for a
// link after the open cannot redirect a write.
type seatDirFD struct {
	fd   int
	path string
}

// openSeatDirectory opens path as a real directory owned by the holder's uid, then tightens it to 0700. O_NOFOLLOW
// refuses a symlink at the final component. The owner and type checks run on the open descriptor, not on a path.
func openSeatDirectory(path string) (*seatDirFD, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open seat directory %s: %w", path, err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, errors.Join(fmt.Errorf("stat seat directory %s: %w", path, err), closeFD(fd))
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return nil, errors.Join(fmt.Errorf("seat directory %s is not a real directory", path), closeFD(fd))
	}
	if uid := currentUID(); int(stat.Uid) != uid {
		return nil, errors.Join(fmt.Errorf("seat directory %s is owned by uid %d, not %d", path, stat.Uid, uid), closeFD(fd))
	}
	if err := unix.Fchmod(fd, 0o700); err != nil {
		return nil, errors.Join(fmt.Errorf("chmod seat directory %s: %w", path, err), closeFD(fd))
	}
	return &seatDirFD{fd: fd, path: path}, nil
}

// WriteFile replaces name inside the directory with data at mode 0600. It unlinks any existing name first, then
// creates the file with O_EXCL|O_NOFOLLOW, so a planted symlink or hardlink is replaced rather than written through.
// It then checks the new descriptor is a regular file with one link, which proves the name is not shared.
func (d *seatDirFD) WriteFile(name string, data []byte) error {
	if err := unix.Unlinkat(d.fd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("remove %s/%s: %w", d.path, name, err)
	}
	fd, err := unix.Openat(d.fd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("create %s/%s: %w", d.path, name, err)
	}
	if err := checkRegularSingleLink(fd, d.path, name); err != nil {
		return errors.Join(err, closeFD(fd))
	}
	file := os.NewFile(uintptr(fd), name)
	if _, err := file.Write(data); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := unix.Fchmod(fd, 0o600); err != nil {
		return errors.Join(err, file.Close())
	}
	return file.Close()
}

// Close releases the directory descriptor.
func (d *seatDirFD) Close() error {
	return closeFD(d.fd)
}

// checkRegularSingleLink refuses a file that is not regular or that has more than one name.
func checkRegularSingleLink(fd int, dir, name string) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("stat %s/%s: %w", dir, name, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("%s/%s is not a regular file", dir, name)
	}
	if uint64(stat.Nlink) != 1 {
		return fmt.Errorf("%s/%s has %d links, want 1", dir, name, stat.Nlink)
	}
	return nil
}

func closeFD(fd int) error {
	return unix.Close(fd)
}
