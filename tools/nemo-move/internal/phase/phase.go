package phase

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type Value string

const (
	Owner   Value = "owner"
	Blocked Value = "blocked"
)

type atomicTempFile interface {
	Name() string
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

type atomicDirectory interface {
	Sync() error
	Close() error
}

type atomicWriteOps struct {
	createTemp func(dir, pattern string) (atomicTempFile, error)
	rename     func(oldPath, newPath string) error
	remove     func(path string) error
	openDir    func(path string) (atomicDirectory, error)
}

func systemAtomicWriteOps() atomicWriteOps {
	return atomicWriteOps{
		createTemp: func(dir, pattern string) (atomicTempFile, error) {
			return os.CreateTemp(dir, pattern)
		},
		rename:  os.Rename,
		remove:  os.Remove,
		openDir: func(path string) (atomicDirectory, error) { return os.Open(path) },
	}
}

// WriteAtomic replaces path with the exact phase bytes using a crash-atomic write.
func WriteAtomic(path string, value Value) error {
	return writeAtomic(path, []byte(value), systemAtomicWriteOps())
}

func writeAtomic(path string, value []byte, ops atomicWriteOps) error {
	dir := filepath.Dir(path)
	file, err := ops.createTemp(dir, ".phase.tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary phase file in %q: %w", dir, err)
	}
	tempPath := file.Name()

	if err := writeComplete(file, value); err != nil {
		return cleanupTemp(file, ops, tempPath, fmt.Errorf("write temporary phase file: %w", err))
	}
	if err := file.Sync(); err != nil {
		return cleanupTemp(file, ops, tempPath, fmt.Errorf("sync temporary phase file: %w", err))
	}
	if err := file.Close(); err != nil {
		return cleanupTempWithoutClose(ops, tempPath, fmt.Errorf("close temporary phase file: %w", err))
	}
	if err := ops.rename(tempPath, path); err != nil {
		return cleanupTempWithoutClose(ops, tempPath, fmt.Errorf("rename temporary phase file to %q: %w", path, err))
	}

	directory, err := ops.openDir(dir)
	if err != nil {
		return fmt.Errorf("open phase directory %q after rename: %w", dir, err)
	}
	if err := directory.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync phase directory %q: %w", dir, err), closeAtomicDirectory(directory, dir))
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close phase directory %q: %w", dir, err)
	}
	return nil
}

func writeComplete(file atomicTempFile, value []byte) error {
	for len(value) > 0 {
		written, err := file.Write(value)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(value) {
			return io.ErrShortWrite
		}
		value = value[written:]
	}
	return nil
}

func cleanupTemp(file atomicTempFile, ops atomicWriteOps, path string, cause error) error {
	closeErr := file.Close()
	if closeErr != nil {
		cause = errors.Join(cause, fmt.Errorf("close temporary phase file during cleanup: %w", closeErr))
	}
	return cleanupTempWithoutClose(ops, path, cause)
}

func cleanupTempWithoutClose(ops atomicWriteOps, path string, cause error) error {
	if err := ops.remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		cause = errors.Join(cause, fmt.Errorf("remove temporary phase file %q: %w", path, err))
	}
	return cause
}

func closeAtomicDirectory(directory atomicDirectory, path string) error {
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close phase directory %q: %w", path, err)
	}
	return nil
}

// Read returns the phase file's exact bytes and preserves read errors.
func Read(path string) (Value, error) {
	data, err := os.ReadFile(path) //nolint:gosec // The CLI's --phase path is an operator-selected local state file.
	if err != nil {
		return "", fmt.Errorf("read phase: %w", err)
	}
	return Value(data), nil
}

// RequireOwner succeeds only when the phase file contains exactly "owner".
func RequireOwner(path string) error {
	value, err := Read(path)
	if err != nil {
		return err
	}
	if value != Owner {
		return fmt.Errorf("phase is %q, not owner", value)
	}
	return nil
}
