package phase

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadAndRequireOwner(t *testing.T) {
	tests := []struct {
		name      string
		contents  []byte
		missing   bool
		isDir     bool
		wantValue Value
		wantOwner bool
	}{
		{name: "owner exact", contents: []byte("owner"), wantValue: Owner, wantOwner: true},
		{name: "owner newline is not exact", contents: []byte("owner\n"), wantValue: Value("owner\n")},
		{name: "empty is blocked", contents: []byte(""), wantValue: Value("")},
		{name: "blocked", contents: []byte("blocked"), wantValue: Blocked},
		{name: "missing is not owner", missing: true},
		{name: "unreadable is not owner", isDir: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "phase")
			if test.isDir {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			} else if !test.missing {
				if err := os.WriteFile(path, test.contents, 0600); err != nil {
					t.Fatal(err)
				}
			}

			value, readErr := Read(path)
			if test.missing || test.isDir {
				if readErr == nil {
					t.Fatal("Read() error = nil, want read failure")
				}
			} else {
				if readErr != nil {
					t.Fatalf("Read() error = %v", readErr)
				}
				if value != test.wantValue {
					t.Fatalf("Read() = %q, want exact bytes %q", value, test.wantValue)
				}
			}

			err := RequireOwner(path)
			if test.wantOwner && err != nil {
				t.Fatalf("RequireOwner() error = %v", err)
			}
			if !test.wantOwner && err == nil {
				t.Fatal("RequireOwner() accepted non-owner phase")
			}
		})
	}
}

func TestWriteAtomicPersistenceFailures(t *testing.T) {
	tests := []struct {
		name     string
		failure  string
		newFinal bool
	}{
		{name: "temporary create failure", failure: "temp-create"},
		{name: "write failure", failure: "write"},
		{name: "file fsync failure", failure: "file-sync"},
		{name: "file close failure", failure: "file-close"},
		{name: "rename failure", failure: "rename"},
		{name: "directory open failure", failure: "dir-open", newFinal: true},
		{name: "directory fsync failure", failure: "dir-sync", newFinal: true},
		{name: "directory close failure", failure: "dir-close", newFinal: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "phase")
			if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
				t.Fatal(err)
			}

			err := writeAtomic(path, []byte("new"), atomicWriteOpsWithFailure(t, test.failure))
			if err == nil {
				t.Fatal("writeAtomic() error = nil, want injected persistence failure")
			}

			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read final file: %v", err)
			}
			want := []byte("old")
			if test.newFinal {
				want = []byte("new")
			}
			if string(got) != string(want) {
				t.Fatalf("final file = %q, want %q", got, want)
			}

			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("read parent directory: %v", err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".phase.tmp-") {
					t.Fatalf("temporary file %q remains after failed write", entry.Name())
				}
			}
		})
	}
}

type atomicFaultTempFile struct {
	*os.File
	failure string
}

func (file atomicFaultTempFile) Write(value []byte) (int, error) {
	if file.failure == "write" {
		return 0, errors.New("injected write failure")
	}
	return file.File.Write(value)
}

func (file atomicFaultTempFile) Sync() error {
	if file.failure == "file-sync" {
		return errors.New("injected file fsync failure")
	}
	return file.File.Sync()
}

func (file atomicFaultTempFile) Close() error {
	err := file.File.Close()
	if file.failure == "file-close" {
		return errors.Join(err, errors.New("injected file close failure"))
	}
	return err
}

type atomicFaultDirectory struct {
	*os.File
	failure string
}

func (directory atomicFaultDirectory) Sync() error {
	if directory.failure == "dir-sync" {
		return errors.New("injected directory fsync failure")
	}
	return directory.File.Sync()
}

func (directory atomicFaultDirectory) Close() error {
	err := directory.File.Close()
	if directory.failure == "dir-close" {
		return errors.Join(err, errors.New("injected directory close failure"))
	}
	return err
}

func atomicWriteOpsWithFailure(t *testing.T, failure string) atomicWriteOps {
	t.Helper()
	return atomicWriteOps{
		createTemp: func(dir, pattern string) (atomicTempFile, error) {
			if failure == "temp-create" {
				return nil, errors.New("injected temporary create failure")
			}
			file, err := os.CreateTemp(dir, pattern)
			if err != nil {
				return nil, err
			}
			return atomicFaultTempFile{File: file, failure: failure}, nil
		},
		rename: func(oldPath, newPath string) error {
			if failure == "rename" {
				return errors.New("injected rename failure")
			}
			return os.Rename(oldPath, newPath)
		},
		remove: os.Remove,
		openDir: func(path string) (atomicDirectory, error) {
			if failure == "dir-open" {
				return nil, errors.New("injected directory open failure")
			}
			file, err := os.Open(path)
			if err != nil {
				return nil, err
			}
			return atomicFaultDirectory{File: file, failure: failure}, nil
		},
	}
}
