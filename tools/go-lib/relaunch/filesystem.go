// SPDX-License-Identifier: MIT OR Apache-2.0

package relaunch

import (
	"errors"
	"io"
	"io/fs"
	"os"
)

// FileSystem is the filesystem surface shared by relaunch and seat-list operations.
type FileSystem interface {
	ReadFile(path string) ([]byte, error)
	ReadDir(path string) ([]fs.DirEntry, error)
	Stat(path string) (fs.FileInfo, error)
	Readlink(path string) (string, error)
	WriteFile(path string, data []byte, perm fs.FileMode) error
	MkdirAll(path string, perm fs.FileMode) error
	Rename(oldpath, newpath string) error
	Remove(path string) error
}

// NodeFileSystem uses the host filesystem.
type NodeFileSystem struct{}

func (NodeFileSystem) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path) // #nosec G304 -- callers supply host state and session paths through this filesystem boundary.
}

func (NodeFileSystem) ReadDir(path string) ([]fs.DirEntry, error) {
	file, err := os.Open(path) // #nosec G304 -- callers supply directories through this host filesystem boundary.
	if err != nil {
		return nil, err
	}
	entries, readErr := file.ReadDir(-1)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	return entries, nil
}

func (NodeFileSystem) ReadPrefix(path string, maxBytes int) ([]byte, error) {
	file, err := os.Open(path) // #nosec G304 -- session scans intentionally read caller-selected host paths.
	if err != nil {
		return nil, err
	}
	data := make([]byte, maxBytes)
	n, readErr := io.ReadFull(file, data)
	closeErr := file.Close()
	if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		return nil, errors.Join(readErr, closeErr)
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return data[:n], nil
}

// ReadTail reads at most the last maxBytes of path.
func (NodeFileSystem) ReadTail(path string, maxBytes int) ([]byte, error) {
	file, err := os.Open(path) // #nosec G304 -- session scans intentionally read caller-selected host paths.
	if err != nil {
		return nil, err
	}
	data, readErr := readTailFrom(file, maxBytes)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	return data, nil
}

func readTailFrom(file *os.File, maxBytes int) ([]byte, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	offset := max(info.Size()-int64(maxBytes), 0)
	data := make([]byte, info.Size()-offset)
	n, err := file.ReadAt(data, offset)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return data[:n], nil
}

func (NodeFileSystem) Stat(path string) (fs.FileInfo, error) {
	return os.Stat(path)
}

func (NodeFileSystem) Readlink(path string) (string, error) {
	return os.Readlink(path)
}

func (NodeFileSystem) WriteFile(path string, data []byte, perm fs.FileMode) error {
	return os.WriteFile(path, data, perm)
}

func (NodeFileSystem) MkdirAll(path string, perm fs.FileMode) error {
	return os.MkdirAll(path, perm)
}

func (NodeFileSystem) Rename(oldpath, newpath string) error {
	return os.Rename(oldpath, newpath)
}

func (NodeFileSystem) Remove(path string) error {
	return os.Remove(path)
}
