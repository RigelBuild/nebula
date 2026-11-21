package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// orphanFixture makes an idle secondary workspace whose .jj/repo pointer names a store that does not exist, as
// after its primary checkout is removed. PATH gets a fake jj that logs each call to the returned file and fails.
func orphanFixture(t *testing.T, name string) (root, dir, argsFile string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir = filepath.Join(root, name)
	mustWrite(t, filepath.Join(dir, jjDir, "repo"), filepath.Join(root, "gone-primary", jjDir, "repo")+"\n")
	mustWrite(t, filepath.Join(dir, "README.md"), "x\n")
	backdate(t, dir, time.Now().Add(-100*time.Hour))
	argsFile = filepath.Join(t.TempDir(), "jj-args")
	bin := fakeBin(t, "jj", `printf '%s\n' "$*" >> '`+argsFile+`'
exit 1
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return root, dir, argsFile
}

// TestOrphanedStoreKeptWithoutJJ checks a workspace whose store is gone is kept, named as orphaned-store with its
// missing store path, and neither deleted nor marked. No jj command runs on it, so the fake jj must log nothing.
func TestOrphanedStoreKeptWithoutJJ(t *testing.T) {
	root, dir, argsFile := orphanFixture(t, "orphan")
	missing := filepath.Join(root, "gone-primary", jjDir, "repo")
	var stdout bytes.Buffer
	kept, err := cleanWorkspaces(t.Context(), config{roots: []string{root}, idle: 48 * time.Hour, apply: true}, nil, &printer{w: &stdout})
	if err != nil {
		t.Fatalf("sweep error: %v\nstdout:\n%s", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), "orphaned-store") || !strings.Contains(stdout.String(), missing) {
		t.Fatalf("orphaned workspace not named with its missing store:\n%s", stdout.String())
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("orphaned workspace removed: %v\nstdout:\n%s", err, stdout.String())
	}
	if _, err := os.Stat(filepath.Join(dir, workspaceMarker)); !os.IsNotExist(err) {
		t.Fatalf("orphaned workspace got a deletion marker: stat err = %v", err)
	}
	if !keptFor(kept, dir) {
		t.Fatalf("orphaned workspace not kept for the report: %+v", kept)
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Fatalf("jj was run on an orphaned workspace: stat err = %v", err)
	}
}

// TestSkipSummaryCountsEachReason checks the sweep ends with one summary line per skip reason, sorted by reason.
// An orphaned store and a jj inspect failure are two reasons, so each gets its own line.
func TestSkipSummaryCountsEachReason(t *testing.T) {
	root, _, _ := orphanFixture(t, "orphan")
	store, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mustMkdir(t, filepath.Join(store, jjDir, "repo"))
	stale := filepath.Join(root, "stale")
	mustWrite(t, filepath.Join(stale, jjDir, "repo"), filepath.Join(store, jjDir, "repo")+"\n")
	mustWrite(t, filepath.Join(stale, "README.md"), "x\n")
	backdate(t, stale, time.Now().Add(-100*time.Hour))
	var stdout bytes.Buffer
	if _, err := cleanWorkspaces(t.Context(), config{roots: []string{root}, idle: 48 * time.Hour, apply: true}, nil, &printer{w: &stdout}); err != nil {
		t.Fatalf("sweep error: %v\nstdout:\n%s", err, stdout.String())
	}
	var summary []string
	for line := range strings.SplitSeq(stdout.String(), "\n") {
		if strings.HasPrefix(line, "skip ") {
			summary = append(summary, line)
		}
	}
	want := []string{
		"skip jj inspect failed: 1 workspace(s), 0.00 GiB, kept in place",
		"skip orphaned-store: 1 workspace(s), 0.00 GiB, kept in place",
	}
	if !slices.Equal(summary, want) {
		t.Fatalf("skip summary = %q, want %q\nstdout:\n%s", summary, want, stdout.String())
	}
}
