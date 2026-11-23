// Package resticenv filters inherited restic configuration.
package resticenv

import (
	"os"
	"strings"
)

// Filter removes inherited restic variables and optionally overrides TMPDIR.
func Filter(tmpDir string) []string {
	environ := os.Environ()
	result := make([]string, 0, len(environ)+1)
	for _, entry := range environ {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "RESTIC_") || (tmpDir != "" && key == "TMPDIR") {
			continue
		}
		result = append(result, entry)
	}
	if tmpDir != "" {
		result = append(result, "TMPDIR="+tmpDir)
	}
	return result
}

// backendPrefixes are the restic repository schemes that do not name a local directory.
var backendPrefixes = []string{"s3:", "rest:", "b2:", "azure:", "gs:", "sftp:", "swift:", "rclone:"}

// IsBackend reports whether repo names a remote restic backend rather than a local path.
func IsBackend(repo string) bool {
	for _, prefix := range backendPrefixes {
		if strings.HasPrefix(repo, prefix) {
			return true
		}
	}
	return false
}
