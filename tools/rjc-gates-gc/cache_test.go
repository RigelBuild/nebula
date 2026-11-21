package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCleanGoBuildCacheOnlyWhenOverLimit(t *testing.T) {
	state := t.TempDir()
	root := filepath.Join(state, "cache", "go-build")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "entry")
	if err := os.WriteFile(path, []byte("warm cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	size, err := allocatedSize(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := cleanGoBuildCache(state); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("under-cap cache entry removed: %v", err)
	}
	if err := cleanGoBuildCacheAtLimit(state, size-1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("over-cap cache entry remains, stat error = %v", err)
	}
}

func TestClearDirectoryRemovesEntries(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "entry"), []byte("cached"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := clearDirectory(root); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("cache still contains %d entries", len(entries))
	}
}

func TestGoBuildCacheKeepsItemsAtLimit(t *testing.T) {
	state := t.TempDir()
	root := filepath.Join(state, "cache", "go-build")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "entry")
	if err := os.WriteFile(path, []byte("cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	size, err := allocatedSize(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := cleanGoBuildCacheAtLimit(state, size); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cache at limit removed: %v", err)
	}
}
