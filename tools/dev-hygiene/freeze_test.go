package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCleanLandedIdleNeverDeletedUnderApply checks a clean, landed, idle workspace survives an apply sweep. Deletion
// of workspaces is frozen, so the sweep reports the workspace and keeps its directory.
func TestCleanLandedIdleNeverDeletedUnderApply(t *testing.T) {
	f := newJJFixture(t)
	dir := f.secondary("frozen")
	backdate(t, dir, time.Now().Add(-100*time.Hour))
	var stdout bytes.Buffer
	kept, err := cleanWorkspaces(t.Context(), sweepConfig(f, true), nil, &printer{w: &stdout})
	if err != nil {
		t.Fatalf("sweep error: %v\nstdout:\n%s", err, stdout.String())
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("clean workspace was removed: %v\nstdout:\n%s", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), "workspace "+dir+": reclaimable") || !keptFor(kept, dir) {
		t.Fatalf("clean workspace not reported: %+v\nstdout:\n%s", kept, stdout.String())
	}
	for _, word := range []string{"would forget", "forgotten and deleted", "deleted"} {
		if strings.Contains(stdout.String(), word) {
			t.Fatalf("delete path reached (%q):\n%s", word, stdout.String())
		}
	}
}

// TestFrozenMarkerDirNeverRemovedUnderApply checks a marked workspace left by an interrupted delete survives an apply
// sweep. Deletion is frozen, so the sweep reports the interrupted delete and keeps the marker.
func TestFrozenMarkerDirNeverRemovedUnderApply(t *testing.T) {
	f := newJJFixture(t)
	dir := f.secondary("marked")
	mustWrite(t, filepath.Join(dir, workspaceMarker), f.primary+"\tmarked\n")
	backdate(t, dir, time.Now().Add(-100*time.Hour))
	var stdout bytes.Buffer
	kept, err := cleanWorkspaces(t.Context(), sweepConfig(f, true), nil, &printer{w: &stdout})
	if err != nil {
		t.Fatalf("sweep error: %v\nstdout:\n%s", err, stdout.String())
	}
	if _, err := os.Stat(filepath.Join(dir, workspaceMarker)); err != nil {
		t.Fatalf("marked workspace or marker removed: %v\nstdout:\n%s", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), "workspace "+dir+": interrupted deletion, report only") || !keptFor(kept, dir) {
		t.Fatalf("marked workspace not reported: %+v\nstdout:\n%s", kept, stdout.String())
	}
	for _, word := range []string{"forgotten and deleted", "removed"} {
		if strings.Contains(stdout.String(), word) {
			t.Fatalf("delete path reached (%q):\n%s", word, stdout.String())
		}
	}
	if got := summaryLines(stdout.String(), "report frozen: "); len(got) != 1 || !strings.HasPrefix(got[0], "report frozen: 1 workspace(s), ") {
		t.Fatalf("frozen summary = %q\nstdout:\n%s", got, stdout.String())
	}
}

// TestFrozenTrashDirNeverRemovedUnderApply checks a leftover trash dir survives an apply sweep. Deletion is frozen, so
// the sweep reports the leftover and keeps the trash directory.
func TestFrozenTrashDirNeverRemovedUnderApply(t *testing.T) {
	f := newJJFixture(t)
	dir := f.secondary("trashed")
	trash := filepath.Join(f.root, trashPrefix+"trashed")
	if err := os.Rename(dir, trash); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(trash, workspaceMarker), f.primary+"\ttrashed\n")
	backdate(t, trash, time.Now().Add(-100*time.Hour))
	var stdout bytes.Buffer
	kept, err := cleanWorkspaces(t.Context(), sweepConfig(f, true), nil, &printer{w: &stdout})
	if err != nil {
		t.Fatalf("sweep error: %v\nstdout:\n%s", err, stdout.String())
	}
	if _, err := os.Stat(trash); err != nil {
		t.Fatalf("trash dir was removed: %v\nstdout:\n%s", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), "workspace "+trash+": leftover trash, report only") || !keptFor(kept, trash) {
		t.Fatalf("trash dir not reported: %+v\nstdout:\n%s", kept, stdout.String())
	}
	if strings.Contains(stdout.String(), "forgotten and deleted") {
		t.Fatalf("delete path reached:\n%s", stdout.String())
	}
}
