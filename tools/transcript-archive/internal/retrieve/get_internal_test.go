package retrieve

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMoveRestoredMissingFile(t *testing.T) {
	err := moveRestored(t.TempDir(), t.TempDir(), "/source", []string{"/source/missing.jsonl"})
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("moveRestored missing file: got %v, want not-exist error", err)
	}
}

func TestRemoveSecretReturnsRemoveError(t *testing.T) {
	const secret = "restic-password-must-not-appear"
	dir := t.TempDir()
	// os.Remove fails on a non-empty directory, and that failure is not ErrNotExist.
	if err := os.WriteFile(filepath.Join(dir, "child"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	err := removeSecret(dir)
	if err == nil {
		t.Fatal("removeSecret on a non-empty directory: got nil, want remove error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("removeSecret error contains the secret: %q", err)
	}
}

func TestRemoveSecretMissingFileIsNotAnError(t *testing.T) {
	if err := removeSecret(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatalf("removeSecret on a missing file: got %v, want nil", err)
	}
}
