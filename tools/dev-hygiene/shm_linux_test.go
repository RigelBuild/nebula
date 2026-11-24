//go:build linux

package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPruneShmOpenFD(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); errors.Is(err, fs.ErrNotExist) {
		t.Skip("proc fd unavailable")
	} else if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dir := filepath.Join(root, "entry")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-12 * time.Hour)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})
	var output bytes.Buffer
	if err := pruneShm(t.Context(), root, 6*time.Hour, true, &printer{w: &output}, defaultShmDeps()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("open file removed: %v", err)
	}
}

func TestShmOnlyApplyFailureSendsFailureDM(t *testing.T) {
	isolateHome(t)
	cotal, argsFile := recordingCotal(t)
	root := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	code := runIsolated(t, []string{"-shm-only", "-apply", "-shm-root", root, "-cotal", cotal, "-to", testRecipient}, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for invalid shm root; stderr %q", code, stderr.String())
	}
	sent, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("no failure DM sent: %v", err)
	}
	if !strings.Contains(string(sent), "failed") || !strings.Contains(string(sent), root) {
		t.Fatalf("failure DM %q does not name the shm failure", sent)
	}
}
