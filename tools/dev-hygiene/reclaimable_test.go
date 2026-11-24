package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// summaryLines returns the end-of-sweep summary lines that start with prefix. Per-workspace lines are excluded, so a count checks the summary.
func summaryLines(stdout, prefix string) []string {
	var lines []string
	for line := range strings.SplitSeq(stdout, "\n") {
		if strings.HasPrefix(line, prefix) {
			lines = append(lines, line)
		}
	}
	return lines
}

// TestIdleOrphanReportedReclaimable checks an orphaned workspace idle past reclaimAge is reported reclaimable, not
// skipped. The clock comes from a tree scan only, so no jj call runs. The summary counts it as reclaimable, not as a skip.
func TestIdleOrphanReportedReclaimable(t *testing.T) {
	root, dir, argsFile := orphanFixture(t, "orphan")
	backdate(t, dir, time.Now().Add(-reclaimAge-time.Hour))
	var stdout bytes.Buffer
	kept, err := cleanWorkspaces(t.Context(), config{roots: []string{root}, idle: 48 * time.Hour}, nil, &printer{w: &stdout})
	if err != nil {
		t.Fatalf("sweep error: %v\nstdout:\n%s", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), "workspace "+dir+": reclaimable") {
		t.Fatalf("idle orphan not reported reclaimable:\n%s", stdout.String())
	}
	if skips := summaryLines(stdout.String(), "skip "); len(skips) != 0 {
		t.Fatalf("reclaimable orphan counted as a skip: %q\nstdout:\n%s", skips, stdout.String())
	}
	if got := summaryLines(stdout.String(), "report reclaimable: "); len(got) != 1 || !strings.HasPrefix(got[0], "report reclaimable: 1 workspace(s), ") {
		t.Fatalf("reclaimable summary = %q\nstdout:\n%s", got, stdout.String())
	}
	if !keptFor(kept, dir) {
		t.Fatalf("reclaimable orphan not kept for the report: %+v", kept)
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Fatalf("jj was run on an orphaned workspace: stat err = %v", err)
	}
}

// TestRecentOrphanStillSkipped checks an orphan idle under reclaimAge keeps its orphaned-store skip.
func TestRecentOrphanStillSkipped(t *testing.T) {
	root, _, _ := orphanFixture(t, "orphan")
	var stdout bytes.Buffer
	if _, err := cleanWorkspaces(t.Context(), config{roots: []string{root}, idle: 48 * time.Hour}, nil, &printer{w: &stdout}); err != nil {
		t.Fatalf("sweep error: %v\nstdout:\n%s", err, stdout.String())
	}
	if got := summaryLines(stdout.String(), "skip "); len(got) != 1 || !strings.HasPrefix(got[0], "skip orphaned-store: 1 workspace(s), ") {
		t.Fatalf("skip summary = %q\nstdout:\n%s", got, stdout.String())
	}
	if got := summaryLines(stdout.String(), "report "); len(got) != 0 {
		t.Fatalf("recent orphan reported as %q\nstdout:\n%s", got, stdout.String())
	}
}

// TestReclaimableNeverDeleted checks no delete path is reachable for a reclaimable workspace, even with apply on.
// An idle orphan and an idle forgotten workspace both stay in place with no marker, and no delete line is printed.
func TestReclaimableNeverDeleted(t *testing.T) {
	f := newJJFixture(t)
	forgot := f.secondary("forgot")
	f.jj(f.primary, "workspace", "forget", "forgot")
	orphan := filepath.Join(f.root, "orphan")
	mustWrite(t, filepath.Join(orphan, jjDir, "repo"), filepath.Join(f.root, "gone-primary", jjDir, "repo")+"\n")
	mustWrite(t, filepath.Join(orphan, "README.md"), "x\n")
	idle := time.Now().Add(-reclaimAge - time.Hour)
	backdate(t, forgot, idle)
	backdate(t, orphan, idle)
	var stdout bytes.Buffer
	if _, err := cleanWorkspaces(t.Context(), sweepConfig(f, true), nil, &printer{w: &stdout}); err != nil {
		t.Fatalf("sweep error: %v\nstdout:\n%s", err, stdout.String())
	}
	for _, dir := range []string{forgot, orphan} {
		if !strings.Contains(stdout.String(), "workspace "+dir+": reclaimable\n") {
			t.Fatalf("%s not reported reclaimable:\n%s", dir, stdout.String())
		}
		if _, err := os.Stat(filepath.Join(dir, "README.md")); err != nil {
			t.Fatalf("reclaimable %s was touched: %v\nstdout:\n%s", dir, err, stdout.String())
		}
		if _, err := os.Stat(filepath.Join(dir, workspaceMarker)); !os.IsNotExist(err) {
			t.Fatalf("reclaimable %s got a deletion marker: stat err = %v", dir, err)
		}
	}
	for _, word := range []string{"would forget", "forgotten and deleted", "deleted"} {
		if strings.Contains(stdout.String(), word) {
			t.Fatalf("delete path reached (%q):\n%s", word, stdout.String())
		}
	}
	if got := summaryLines(stdout.String(), "report reclaimable: "); len(got) != 1 || !strings.HasPrefix(got[0], "report reclaimable: 2 workspace(s), ") {
		t.Fatalf("reclaimable summary = %q\nstdout:\n%s", got, stdout.String())
	}
}
