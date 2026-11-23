package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestFinishMarkedDropsStaleMarkerOnly exercises the helper directly, outside the report-only sweep.
func TestFinishMarkedDropsStaleMarkerOnly(t *testing.T) {
	f := newJJFixture(t)
	dir := f.secondary("stale-rooted")
	mustWrite(t, filepath.Join(dir, "work.txt"), "local only\n")
	mustWrite(t, filepath.Join(dir, workspaceMarker), f.primary+"\tstale-rooted\n")
	backdate(t, dir, time.Now().Add(-100*time.Hour))
	var stdout bytes.Buffer
	reclassify, err := finishMarked(t.Context(), dir, &printer{w: &stdout})
	if err != nil {
		t.Fatalf("finishMarked: %v\nstdout:\n%s", err, stdout.String())
	}
	if !reclassify {
		t.Fatalf("stale marker not reclassified: stdout\n%s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(dir, workspaceMarker)); !os.IsNotExist(err) {
		t.Fatalf("stale marker not dropped: stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "work.txt")); err != nil {
		t.Fatalf("rooted workspace removed by finishMarked: %v", err)
	}
}
