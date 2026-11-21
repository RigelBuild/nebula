package retrieve

import (
	"errors"
	"os"
	"testing"
)

func TestMoveRestoredMissingFile(t *testing.T) {
	err := moveRestored(t.TempDir(), t.TempDir(), "/source", []string{"/source/missing.jsonl"})
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("moveRestored missing file: got %v, want not-exist error", err)
	}
}
