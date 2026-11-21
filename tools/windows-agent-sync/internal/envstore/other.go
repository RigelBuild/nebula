//go:build !windows

package envstore

import (
	"errors"
	"os"
)

// New opens the current user's environment store.
func New() (Store, error) {
	return nil, errors.New("windows environment store is unavailable on this platform")
}

// ReplaceFile atomically replaces dst with src.
func ReplaceFile(src, dst string) error {
	return os.Rename(src, dst)
}
