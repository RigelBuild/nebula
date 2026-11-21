package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestExecCommandIgnoresRelativeOverlayEntries(t *testing.T) {
	t.Parallel()
	// sh is on the process PATH, so a relative or empty overlay entry that fell back to it would find it.
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	for _, path := range []string{"", ":", ".", "bin"} {
		r := &repository{env: []string{"PATH=" + path}}
		if cmd := r.execCommand(t.Context(), "sh"); !errors.Is(cmd.Err, exec.ErrNotFound) {
			t.Errorf("PATH=%q: resolved %q, err %v; want not found", path, cmd.Path, cmd.Err)
		}
	}
	overlayBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(overlayBin, "sh"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	r := &repository{env: []string{"PATH=:" + overlayBin}}
	if cmd := r.execCommand(t.Context(), "sh"); cmd.Err != nil || cmd.Path != filepath.Join(overlayBin, "sh") {
		t.Fatalf("absolute overlay: resolved %q, err %v", cmd.Path, cmd.Err)
	}
}
