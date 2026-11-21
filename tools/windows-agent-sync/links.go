package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/samber/oops"
)

// link maps one repo-relative source to a destination. The table comes from the
// layout's linkTable file.
type link struct {
	Src string `json:"src"`
	Dst string `json:"dst"`
}

type linkFS interface {
	Lstat(name string) (os.FileInfo, error)
	Readlink(name string) (string, error)
	MkdirAll(path string, perm os.FileMode) error
	Rename(oldpath, newpath string) error
	Remove(name string) error
	Symlink(oldname, newname string) error
}

type osLinkFS struct{}

func (osLinkFS) Lstat(name string) (os.FileInfo, error) { return os.Lstat(name) }
func (osLinkFS) Readlink(name string) (string, error)   { return os.Readlink(name) }
func (osLinkFS) MkdirAll(path string, perm os.FileMode) error {
	return os.MkdirAll(path, perm)
}
func (osLinkFS) Rename(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }
func (osLinkFS) Remove(name string) error             { return os.Remove(name) }
func (osLinkFS) Symlink(oldname, newname string) error {
	return os.Symlink(oldname, newname)
}

func syncLinks(co checkout, home, appData string, table []link, fs linkFS) error {
	for _, managed := range table {
		source, err := filepath.Abs(co.path(managed.Src))
		if err != nil {
			return oops.In("windows-agent-sync").Wrapf(err, "resolving link source %s", managed.Src)
		}
		target, err := linkTarget(home, appData, managed.Dst)
		if err != nil {
			return err
		}
		if err := syncLink(fs, source, target); err != nil {
			return oops.In("windows-agent-sync").Wrapf(err, "syncing link %s", managed.Dst)
		}
	}
	return nil
}

func linkTarget(home, appData, destination string) (string, error) {
	const appDataPrefix = "%APPDATA%/"
	if strings.HasPrefix(destination, appDataPrefix) {
		if appData == "" {
			return "", oops.In("windows-agent-sync").Errorf("APPDATA is required for jj config link")
		}
		return filepath.Join(appData, filepath.FromSlash(strings.TrimPrefix(destination, appDataPrefix))), nil
	}
	return filepath.Join(home, filepath.FromSlash(destination)), nil
}

func syncLink(fs linkFS, source, target string) error {
	if err := fs.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return fmt.Errorf("creating link directory: %w", err)
	}
	done, err := clearTarget(fs, source, target)
	if err != nil || done {
		return err
	}
	if err := fs.Symlink(source, target); err != nil {
		return fmt.Errorf("creating link: %w", err)
	}
	return nil
}

// clearTarget reports done when target already links to source; otherwise it removes
// a wrong link or moves a non-link to <target>.hm-backup.
func clearTarget(fs linkFS, source, target string) (bool, error) {
	info, err := fs.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking link target: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		current, err := fs.Readlink(target)
		if err != nil {
			return false, fmt.Errorf("reading existing link: %w", err)
		}
		if sameLinkTarget(current, source) {
			return true, nil
		}
		if err := fs.Remove(target); err != nil {
			return false, fmt.Errorf("removing incorrect link: %w", err)
		}
		return false, nil
	}
	backup := target + ".hm-backup"
	if _, err := fs.Lstat(backup); err == nil {
		return false, fmt.Errorf("backup already exists at %s", backup)
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("checking backup path: %w", err)
	}
	if err := fs.Rename(target, backup); err != nil {
		return false, fmt.Errorf("moving existing target to backup: %w", err)
	}
	return false, nil
}

func sameLinkTarget(left, right string) bool {
	clean := func(path string) string {
		path = strings.ReplaceAll(path, `\`, "/")
		return strings.ToLower(filepath.Clean(path))
	}
	return clean(left) == clean(right)
}
