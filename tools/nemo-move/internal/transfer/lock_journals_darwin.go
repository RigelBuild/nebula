//go:build darwin

package transfer

import (
	"errors"
	"strings"
)

// LockJournals is unavailable on macOS because the Linux append-only inode flag is not supported.
func LockJournals(string) error {
	return errors.New("locking paging journals requires a Linux filesystem with append-only flag support")
}

// ReplaceJournals requires Linux append-only inode flags.
func ReplaceJournals(_ string, manifest Manifest, _ []string) error {
	for _, entry := range manifest.Entries {
		if entry.Type == fileType && strings.HasPrefix(entry.Path, pageRoot+"/") {
			return errors.New("replacing paging journals requires a Linux filesystem with append-only flag support")
		}
	}
	return nil
}

func verifyLink(_, _ string, _ []string) error {
	return errors.New("rooted symlink verification requires Linux")
}
