//go:build linux

package transfer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func verifyLink(root, path string, roots []string) (result error) {
	rootFD, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open transfer root: %w", err)
	}
	defer func() { result = errors.Join(result, unix.Close(rootFD)) }()
	rootPath, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(rootFD))
	if err != nil {
		return fmt.Errorf("resolve transfer root: %w", err)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return errors.New("symlink path outside transfer root")
	}
	fd, err := unix.Openat2(rootFD, rel, &unix.OpenHow{Flags: unix.O_PATH | unix.O_CLOEXEC, Resolve: unix.RESOLVE_IN_ROOT})
	if err != nil {
		return fmt.Errorf("open rooted symlink: %w", err)
	}
	defer func() { result = errors.Join(result, unix.Close(fd)) }()
	resolved, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil {
		return fmt.Errorf("read resolved symlink: %w", err)
	}
	for _, declared := range roots {
		rel, err := filepath.Rel(filepath.Join(rootPath, declared), resolved)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return nil
		}
	}
	return errors.New("symlink escapes manifest roots")
}
