//go:build !windows

package envstore

import (
	"errors"
	"os"
	"path/filepath"
)

// MkdirPrivate creates dir readable only by the current user and tightens an
// existing dir to the same mode.
func MkdirPrivate(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// CreatePrivateFile creates a new file in dir, named by pattern with its "*"
// replaced by a random string, readable only by the current user.
func CreatePrivateFile(dir, pattern string) (*os.File, error) {
	for range privateNameAttempts {
		name, err := privateTempName(pattern)
		if err != nil {
			return nil, err
		}
		file, err := os.OpenFile(filepath.Join(dir, name), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // dir is the caller's private export directory.
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return file, err
	}
	return nil, errPrivateNameExhausted
}
