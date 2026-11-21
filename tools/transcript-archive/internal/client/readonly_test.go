package client

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/inventory"
)

// stateTree records each entry's path, mode, size, mtime and, for regular files, SHA-256.
// A chmod that keeps the current mode changes only ctime, which it does not record.
func stateTree(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s %v %d %d", rel, info.Mode(), info.Size(), info.ModTime().UnixNano())
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(&b, " %x", sha256.Sum256(data))
		}
		b.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// seedReadOnlyState writes a real lifecycle state and plants the leftovers a read-write Open would clean up.
func seedReadOnlyState(t *testing.T) (state, repo string, newestDelta string) {
	t.Helper()
	repo = filepath.Join(t.TempDir(), "repo")
	c, _, state := openTestClient(t, repo)
	line := inventory.Line{Path: "first", Type: "file", Size: 1}
	if err := c.Scan(t.Context(), inventory.Inventory{Lines: []inventory.Line{line}}, inventory.Delta{Added: []inventory.Line{line}}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".current-orphan", ".password-orphan", "scan-01ARZ3NDEKTSV4RRFFQ69G5FAV.jsonl"} {
		if err := os.WriteFile(filepath.Join(state, name), []byte("leftover\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return state, repo, c.newestDelta
}

func TestOpenReadOnlyLeavesStateDirUnchanged(t *testing.T) {
	state, repo, newestDelta := seedReadOnlyState(t)
	// Open sets these to 0o700 and 0o600; seed other modes so a regression changes the tree.
	if err := os.Chmod(state, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(state, "journal.jsonl"), 0o640); err != nil {
		t.Fatal(err)
	}
	before := stateTree(t, state)
	ro, err := OpenReadOnly(state, RepoRef{Scope: "k3s-test-one", Repository: repo})
	if err != nil {
		t.Fatal(err)
	}
	if after := stateTree(t, state); after != before {
		t.Fatalf("read-only open changed the state directory\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if ro.newestDelta != newestDelta {
		t.Fatalf("read-only newestDelta = %q, want %q", ro.newestDelta, newestDelta)
	}
	if _, err := os.Stat(filepath.Join(state, "scan-01ARZ3NDEKTSV4RRFFQ69G5FAV.jsonl")); err != nil {
		t.Fatalf("orphan scan removed by read-only open: %v", err)
	}
}

func TestOpenReadOnlyEmptyStateDirWritesNothing(t *testing.T) {
	state := t.TempDir()
	before := stateTree(t, state)
	ro, err := OpenReadOnly(state, RepoRef{Scope: "k3s-test-one", Repository: filepath.Join(t.TempDir(), "repo")})
	if err != nil {
		t.Fatal(err)
	}
	if after := stateTree(t, state); after != before {
		t.Fatalf("read-only open of empty state wrote files\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if oldest, bytes := ro.Pending(); !oldest.IsZero() || bytes != 0 {
		t.Fatalf("pending = %v %d, want none", oldest, bytes)
	}
}

func TestOpenReadOnlyMissingStateDirIsNotCreated(t *testing.T) {
	state := filepath.Join(t.TempDir(), "missing")
	_, err := OpenReadOnly(state, RepoRef{Scope: "k3s-test-one", Repository: filepath.Join(t.TempDir(), "repo")})
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("OpenReadOnly error = %v, want fs.ErrNotExist", err)
	}
	if _, err := os.Stat(state); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing state directory was created: %v", err)
	}
}

func TestReadOnlyClientRejectsWrites(t *testing.T) {
	state, repo, _ := seedReadOnlyState(t)
	before := stateTree(t, state)
	ro, err := OpenReadOnly(state, RepoRef{Scope: "k3s-test-one", Repository: repo})
	if err != nil {
		t.Fatal(err)
	}
	line := inventory.Line{Path: "second", Type: "file", Size: 2}
	if err := ro.Scan(t.Context(), inventory.Inventory{Lines: []inventory.Line{line}}, inventory.Delta{Added: []inventory.Line{line}}); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("Scan error = %v, want ErrReadOnly", err)
	}
	if _, err := ro.Pass(t.Context(), nil, nil); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("Pass error = %v, want ErrReadOnly", err)
	}
	if _, err := ro.PassFinal(t.Context(), nil); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("PassFinal error = %v, want ErrReadOnly", err)
	}
	if err := ro.Recover(t.Context()); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("Recover error = %v, want ErrReadOnly", err)
	}
	if after := stateTree(t, state); after != before {
		t.Fatalf("rejected write changed the state directory\nbefore:\n%s\nafter:\n%s", before, after)
	}
}
