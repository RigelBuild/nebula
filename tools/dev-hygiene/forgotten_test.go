package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestForgottenWorkspaceNotInspectFailure checks a forgotten workspace is reported, not skipped.
func TestForgottenWorkspaceNotInspectFailure(t *testing.T) {
	f := newJJFixture(t)
	dir := f.secondary("forgot")
	f.jj(f.primary, "workspace", "forget", "forgot")
	backdate(t, dir, time.Now().Add(-100*time.Hour))
	var stdout bytes.Buffer
	kept, err := cleanWorkspaces(t.Context(), sweepConfig(f, false), nil, &printer{w: &stdout})
	if err != nil {
		t.Fatalf("sweep error: %v\nstdout:\n%s", err, stdout.String())
	}
	if strings.Contains(stdout.String(), "jj inspect failed") {
		t.Fatalf("forgotten workspace counted as an inspect failure:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), dir+": forgotten\n") {
		t.Fatalf("forgotten workspace not named as forgotten:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "report forgotten: 1 workspace(s)") || strings.Contains(stdout.String(), "skip forgotten") {
		t.Fatalf("forgotten workspace not counted as a report, or counted as a skip:\n%s", stdout.String())
	}
	if !keptFor(kept, dir) {
		t.Fatalf("forgotten workspace not kept for the report: %+v", kept)
	}
	if _, err := os.Stat(filepath.Join(dir, "README.md")); err != nil {
		t.Fatalf("forgotten workspace was touched: %v", err)
	}
}
