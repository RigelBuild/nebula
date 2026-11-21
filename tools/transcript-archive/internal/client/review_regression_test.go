package client

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/inventory"
)

func TestReviewScanFailureKeepsCommittedState(t *testing.T) {
	c, _, _ := openTestClient(t, filepath.Join(t.TempDir(), "repo"))
	file := filepath.Join(t.TempDir(), "file")
	line := writeVersion(t, file, "first", "normal")
	scanLines(t, c, line)
	oldID, oldStarted := c.scanID, c.scanStarted
	c.scanHook = func(point string) error {
		if point == "journal" {
			return errors.New("injected")
		}
		return nil
	}
	if err := c.Scan(t.Context(), inventory.Inventory{}, inventory.Delta{Removed: []inventory.Line{line}}); err == nil {
		t.Fatal("scan succeeded")
	}
	if c.scanID != oldID || c.scanStarted != oldStarted || len(c.scanned) != 1 || len(c.pending) != 1 {
		t.Fatal("failed scan changed committed state")
	}
	c.scanHook = nil
	if _, err := c.Pass(t.Context(), []string{file}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestReviewOnePassPerScan(t *testing.T) {
	c, _, _ := openTestClient(t, filepath.Join(t.TempDir(), "repo"))
	scanLines(t, c)
	if _, err := c.Pass(t.Context(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Pass(t.Context(), nil, nil); !errors.Is(err, ErrPassDone) {
		t.Fatalf("second pass: %v", err)
	}
	if _, err := c.PassFinal(t.Context(), nil); !errors.Is(err, ErrPassDone) {
		t.Fatalf("final pass: %v", err)
	}
}

func TestReviewScratchRequired(t *testing.T) {
	_, err := Open(filepath.Join(t.TempDir(), "state"), RepoRef{Scope: "scope", Repository: "repo"}, &fakeEscrow{})
	if err == nil {
		t.Fatal("missing scratch accepted")
	}
}

func TestReviewPrivateErrorsHideStatePath(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "sentinel-private-state")
	if err := os.Mkdir(secret, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(secret, "password"), 0700); err != nil {
		t.Fatal(err)
	}
	c := &Client{stateDir: secret, scratch: filepath.Join(secret, "password", "child")}
	_, err := c.readOrCreatePassword()
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("password error leaks state path: %v", err)
	}
	c.stateDir = filepath.Join(secret, "missing")
	_, err = c.privateList("test", nil)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("list error leaks state path: %v", err)
	}
	c.scratch = filepath.Join(secret, "password", "child", "nested")
	if err := os.WriteFile(filepath.Join(secret, "password", "child"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = c.runJSON(t.Context())
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("scratch error leaks state path: %v", err)
	}
}

func TestReviewCleanOldScanAndTemps(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	c, _, state := openTestClient(t, repo)
	scanLines(t, c)
	old := filepath.Join(state, "scan-"+c.scanID+".jsonl")
	c.scanHook = func(point string) error {
		if point == "commit" {
			return errors.New("crash")
		}
		return nil
	}
	if err := c.Scan(t.Context(), inventory.Inventory{}, inventory.Delta{}); err == nil {
		t.Fatal("expected crash")
	}
	for _, name := range []string{".scan-left", ".current-left", ".password-left", "deltas/.scan-left"} {
		if err := os.WriteFile(filepath.Join(state, name), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	reopenTestClient(t, state, repo)
	for _, name := range []string{old, filepath.Join(state, ".scan-left"), filepath.Join(state, ".current-left"), filepath.Join(state, ".password-left"), filepath.Join(state, "deltas/.scan-left")} {
		if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("orphan remains: %s: %v", name, err)
		}
	}
}
