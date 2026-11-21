//go:build linux

package transfer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const appendFlag = 0x00000020

// LockJournals sets FS_APPEND_FL on every existing JSONL journal in dir.
func LockJournals(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("list journal directory: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		file, err := os.OpenFile(path, os.O_RDONLY, 0) //nolint:gosec // Journal path is selected from the operator-specified directory.
		if err != nil {
			return fmt.Errorf("open journal %s: %w", entry.Name(), err)
		}
		flags, getErr := getFileFlags(file.Fd())
		if getErr == nil {
			getErr = setFileFlags(file.Fd(), flags|appendFlag)
		}
		closeErr := file.Close()
		if err := errors.Join(getErr, closeErr); err != nil {
			return fmt.Errorf("lock journal %s: %w", entry.Name(), err)
		}
	}
	return nil
}

func getFileFlags(fd uintptr) (uint32, error) {
	flags, err := unix.IoctlGetInt(int(fd), unix.FS_IOC_GETFLAGS)
	return uint32(flags), err //nolint:gosec // Filesystem flags are a 32-bit bitmask.
}

func setFileFlags(fd uintptr, flags uint32) error {
	return unix.IoctlSetPointerInt(int(fd), unix.FS_IOC_SETFLAGS, int(flags))
}

// ReplaceJournals installs staged journals while the paging service is stopped.
// On first run, with no journal in the manifest and no initialized directory,
// it creates each name in defaults as an empty journal.
func ReplaceJournals(root string, manifest Manifest, defaults []string) (resultErr error) {
	var journals []string
	for _, entry := range manifest.Entries {
		if entry.Type == fileType && strings.HasPrefix(entry.Path, pageRoot+"/") {
			if err := validateRelativePath(entry.Path); err != nil {
				return err
			}
			name := strings.TrimPrefix(entry.Path, pageRoot+"/")
			if strings.Contains(name, "/") || !strings.HasSuffix(name, ".jsonl") {
				return fmt.Errorf("invalid journal %s", entry.Path)
			}
			journals = append(journals, name)
		}
	}
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open transfer root: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, unix.Close(rootFD)) }()
	dir, _, err := openParent(rootFD, pageRoot+"/.initialized", true)
	if err != nil {
		return fmt.Errorf("open journal directory: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, unix.Close(dir)) }()
	if err := errors.Join(journalSystemOps.own(dir, 0, 0), unix.Fchmod(dir, 0o755)); err != nil {
		return fmt.Errorf("set journal directory ownership: %w", err)
	}
	var marker unix.Stat_t
	err = unix.Fstatat(dir, ".initialized", &marker, unix.AT_SYMLINK_NOFOLLOW)
	initialized := err == nil
	if err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("inspect initialization marker: %w", err)
	}
	if initialized && marker.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("initialization marker is not a regular file")
	}
	if len(journals) == 0 && !initialized {
		journals = defaults
	}
	for _, name := range journals {
		if err := replaceJournal(dir, name, initialized, journalSystemOps); err != nil {
			return fmt.Errorf("replace journal %s: %w", name, err)
		}
	}
	if !initialized {
		fd, err := unix.Openat(dir, ".initialized", unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_WRONLY|unix.O_CLOEXEC, 0o444)
		if err != nil {
			return fmt.Errorf("create initialization marker: %w", err)
		}
		err = errors.Join(journalSystemOps.own(fd, 0, 0), unix.Fchmod(fd, 0o444), unix.Fsync(fd), unix.Close(fd))
		if err != nil {
			return fmt.Errorf("write initialization marker: %w", errors.Join(err, unix.Unlinkat(dir, ".initialized", 0), unix.Fsync(dir)))
		}
	}
	if err := unix.Fsync(dir); err != nil {
		return fmt.Errorf("sync journal directory: %w", err)
	}
	return nil
}

type journalOps struct {
	get func(uintptr) (uint32, error)
	set func(uintptr, uint32) error
	own func(int, int, int) error
}

var journalSystemOps = journalOps{get: getFileFlags, set: setFileFlags, own: unix.Fchown}

func replaceJournal(dir int, name string, initialized bool, ops journalOps) (resultErr error) {
	old, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	if errors.Is(err, unix.ENOENT) && initialized {
		return fmt.Errorf("initialized journal missing: %w", err)
	}
	if old >= 0 {
		defer func() { resultErr = errors.Join(resultErr, unix.Close(old)) }()
	}
	var flags uint32
	if old >= 0 {
		var st unix.Stat_t
		if err := unix.Fstat(old, &st); err != nil {
			return err
		}
		if st.Mode&unix.S_IFMT != unix.S_IFREG {
			return errors.New("journal is not a regular file")
		}
		flags, err = ops.get(uintptr(old))
		if err != nil {
			return err
		}
	}
	if !initialized && old < 0 {
		fd, createErr := unix.Openat(dir, name+".incoming", unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_WRONLY|unix.O_CLOEXEC, 0o600)
		if createErr == nil {
			if closeErr := unix.Close(fd); closeErr != nil {
				return closeErr
			}
		} else if !errors.Is(createErr, unix.EEXIST) {
			return createErr
		}
	}
	incoming, err := unix.Openat(dir, name+".incoming", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, unix.Close(incoming)) }()
	var st unix.Stat_t
	if err := unix.Fstat(incoming, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("incoming journal is not a regular file")
	}
	if err := ops.own(incoming, 10000, 10000); err != nil {
		return err
	}
	if err := unix.Fchmod(incoming, 0o600); err != nil {
		return err
	}
	if err := unix.Fsync(incoming); err != nil {
		return err
	}
	if old >= 0 && flags&appendFlag != 0 {
		if err := ops.set(uintptr(old), flags&^appendFlag); err != nil {
			return err
		}
		defer func() {
			if resultErr != nil {
				resultErr = errors.Join(resultErr, ops.set(uintptr(old), flags))
			}
		}()
	}
	installed := false
	defer func() {
		if resultErr != nil && installed {
			current, getErr := ops.get(uintptr(incoming))
			if getErr == nil {
				getErr = ops.set(uintptr(incoming), current|appendFlag)
			}
			resultErr = errors.Join(resultErr, getErr, unix.Fsync(incoming), unix.Fsync(dir))
		}
	}()
	if err := unix.Renameat(dir, name+".incoming", dir, name); err != nil {
		return err
	}
	installed = true
	newFlags, err := ops.get(uintptr(incoming))
	if err != nil {
		return err
	}
	if err := ops.set(uintptr(incoming), newFlags|appendFlag); err != nil {
		return err
	}
	newFlags, err = ops.get(uintptr(incoming))
	if err != nil {
		return err
	}
	if newFlags&appendFlag == 0 {
		return errors.New("journal append flag not set")
	}
	if err := unix.Fsync(incoming); err != nil {
		return err
	}
	return unix.Fsync(dir)
}
