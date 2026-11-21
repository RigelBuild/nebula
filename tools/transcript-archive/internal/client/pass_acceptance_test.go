package client

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/config"
	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/coverage"
	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/inventory"
)

func writeVersion(t *testing.T, path, value, class string) inventory.Line {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	return inventory.Line{Path: path, Type: "file", Size: int64(len(value)), SHA256: sha256.Sum256([]byte(value)), Class: class}
}

func scanLines(t *testing.T, c *Client, lines ...inventory.Line) {
	t.Helper()
	if err := c.Scan(t.Context(), inventory.Inventory{Lines: lines}, inventory.Delta{Changed: lines}); err != nil {
		t.Fatal(err)
	}
}

func ackFiles(t *testing.T, c *Client, paths ...string) Outcome {
	t.Helper()
	result, err := c.Pass(t.Context(), paths, nil)
	if err != nil || result.Kind != Acked {
		t.Fatalf("pass = %#v, %v", result, err)
	}
	return result
}

func TestPendingThirtyMinutes(t *testing.T) {
	localRestic(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c, _, _ := openTestClient(t, filepath.Join(t.TempDir(), "repo"), WithClock(func() time.Time { return now }))
	dir := t.TempDir()
	file, db := filepath.Join(dir, "session.jsonl"), filepath.Join(dir, "index.db")
	for minute := 1; minute <= 30; minute++ {
		now = now.Add(time.Minute)
		line := writeVersion(t, file, fmt.Sprintf("version-%02d", minute), "normal")
		database := writeVersion(t, db, fmt.Sprintf("db-%02d", minute), "final-only")
		if minute%5 == 0 {
			scanLines(t, c, line, database)
			oldest, size := c.Pending()
			if oldest.IsZero() || now.Sub(oldest) > 10*time.Minute || size != line.Size {
				t.Fatalf("minute %d: pending %v/%d", minute, oldest, size)
			}
			ackFiles(t, c, file)
		}
		oldest, size := c.Pending()
		if !oldest.IsZero() && now.Sub(oldest) > 10*time.Minute {
			t.Fatalf("minute %d: oldest %v", minute, oldest)
		}
		if size != 0 {
			t.Fatalf("minute %d: pending bytes %d", minute, size)
		}
	}
}

func TestChangedBetweenScanAndPass(t *testing.T) {
	localRestic(t)
	c, _, _ := openTestClient(t, filepath.Join(t.TempDir(), "repo"))
	path := filepath.Join(t.TempDir(), "session.jsonl")
	first := writeVersion(t, path, "first", "normal")
	scanLines(t, c, first)
	second := writeVersion(t, path, "second version", "normal")
	ackFiles(t, c, path)
	scanLines(t, c, second)
	oldest, size := c.Pending()
	if oldest.IsZero() || size != second.Size {
		t.Fatalf("changed pending %v/%d", oldest, size)
	}
	ackFiles(t, c, path)
	oldest, size = c.Pending()
	if !oldest.IsZero() || size != 0 {
		t.Fatalf("pending after ack %v/%d", oldest, size)
	}
}

func TestCoverageAppendBetweenScanAndPass(t *testing.T) {
	localRestic(t)
	repo := filepath.Join(t.TempDir(), "repo")
	c, _, state := openTestClient(t, repo)
	path := filepath.Join(t.TempDir(), "session.jsonl")
	first := writeVersion(t, path, "first", "normal")
	scanLines(t, c, first)
	passID := c.scanID
	writeVersion(t, path, "first appended", "normal")
	ack := ackFiles(t, c, path)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	files := snapshotFiles(t, repo, state, ack.SnapshotID)
	if !files[path] {
		t.Fatal("snapshot does not hold appended path")
	}
	delta, err := os.Open(c.newestDelta)
	if err != nil {
		t.Fatal(err)
	}
	defer delta.Close()
	history := []coverage.SnapshotListing{{SnapshotID: ack.SnapshotID, PassID: passID, Outcome: "pass-acked", PathsOnly: true, Files: map[string][32]byte{path: {}}}}
	report := coverage.Check(inventory.Inventory{}, inventory.Inventory{}, []coverage.Delta{{PassID: passID, R: delta}}, false, coverage.SnapshotListing{}, nil, history, nil)
	if !report.Pass || len(report.History) != 1 || report.History[0].SHA256 != first.SHA256 {
		t.Fatalf("coverage = %#v", report)
	}
}

func TestFinalPassContainsWholeDelta(t *testing.T) {
	localRestic(t)
	repo := filepath.Join(t.TempDir(), "repo")
	c, _, state := openTestClient(t, repo)
	dir := t.TempDir()
	lines := make([]inventory.Line, 0, 4)
	var parent Outcome
	for i := range 3 {
		path := filepath.Join(dir, fmt.Sprintf("session-%d.jsonl", i))
		lines = append(lines, writeVersion(t, path, fmt.Sprintf("data-%d", i), "normal"))
		scanLines(t, c, lines...)
		parent = ackFiles(t, c, path)
	}
	db := writeVersion(t, filepath.Join(dir, "index.db"), "database", "final-only")
	lines = append(lines, db)
	scanLines(t, c, lines...)
	parentFiles := snapshotFiles(t, repo, state, parent.SnapshotID)
	wantUnmodified := 0
	for _, line := range lines {
		if parentFiles[line.Path] {
			wantUnmodified++
		}
	}
	if wantUnmodified == 0 {
		t.Fatal("parent snapshot contains no unchanged delta files")
	}
	result, err := c.PassFinal(t.Context(), []string{"final"})
	if err != nil || result.Kind != Acked {
		t.Fatalf("final pass = %#v, %v", result, err)
	}
	if result.FilesUnmodified != wantUnmodified {
		t.Fatalf("final files_unmodified = %d, want %d", result.FilesUnmodified, wantUnmodified)
	}
	finalFiles := snapshotFiles(t, repo, state, result.SnapshotID)
	for _, line := range lines {
		if !finalFiles[line.Path] {
			t.Errorf("snapshot missing %s", line.Path)
		}
	}
}

func snapshotFiles(t *testing.T, repo, state, snapshot string) map[string]bool {
	t.Helper()
	args := []string{"-r", repo, "--password-file", filepath.Join(state, "password"), "ls", "--json", "--no-lock", snapshot}
	output, err := exec.Command("restic", args...).Output()
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]bool{}
	decoder := json.NewDecoder(bytes.NewReader(output))
	for decoder.More() {
		var entry struct {
			Type string `json:"type"`
			Path string `json:"path"`
		}
		if err := decoder.Decode(&entry); err != nil {
			t.Fatal(err)
		}
		if entry.Type == "file" {
			files[entry.Path] = true
		}
	}
	return files
}

func TestWalkScanFinalCoverage(t *testing.T) {
	localRestic(t)
	root := t.TempDir()
	path := filepath.Join(root, "data", "transcript")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	writeVersion(t, path, "transcript", "normal")
	cfg := config.Walk{Root: root, Sources: []config.Source{{Name: "data", Path: "data"}}}
	final, delta, err := inventory.Walk(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(t.TempDir(), "repo")
	state := filepath.Join(root, ".example", "archive", "test")
	c, err := Open(state, RepoRef{Scope: "k3s-test-one", Repository: repo}, &fakeEscrow{}, WithScratch(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Scan(t.Context(), final, delta); err != nil {
		t.Fatal(err)
	}
	first, err := c.Pass(t.Context(), []string{path}, nil)
	if err != nil || first.Kind != Acked || len(first.Vanished) != 0 {
		t.Fatalf("pass = %#v, %v", first, err)
	}
	if err := c.Scan(t.Context(), final, inventory.Delta{}); err != nil {
		t.Fatal(err)
	}
	last, err := c.PassFinal(t.Context(), nil)
	if err != nil || last.Kind != Acked || len(last.Vanished) != 0 {
		t.Fatalf("final pass = %#v, %v", last, err)
	}
	files := snapshotDigests(t, repo, state, last.SnapshotID)
	deltaFile, err := os.Open(c.newestDelta)
	if err != nil {
		t.Fatal(err)
	}
	defer deltaFile.Close()
	listing := coverage.SnapshotListing{Files: files, StateFiles: map[string]struct{}{c.newestDelta: {}}}
	report := coverage.Check(final, inventory.Inventory{}, []coverage.Delta{{PassID: c.scanID, R: deltaFile}}, false, listing, nil, nil, nil)
	if !report.Pass || report.Vanished != 0 {
		t.Fatalf("coverage = %#v", report)
	}
}

func TestWalkScanDeletedVersionUnexplained(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "data", "transcript")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	writeVersion(t, path, "transcript", "normal")
	cfg := config.Walk{Root: root, Sources: []config.Source{{Name: "data", Path: "data"}}}
	scan, delta, err := inventory.Walk(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, _, _ := openTestClient(t, filepath.Join(t.TempDir(), "repo"))
	if err := c.Scan(t.Context(), scan, delta); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	final, _, err := inventory.Walk(cfg, &scan)
	if err != nil {
		t.Fatal(err)
	}
	deltaFile, err := os.Open(c.newestDelta)
	if err != nil {
		t.Fatal(err)
	}
	defer deltaFile.Close()
	report := coverage.Check(final, inventory.Inventory{}, []coverage.Delta{{PassID: c.scanID, R: deltaFile}}, false, coverage.SnapshotListing{}, nil, nil, nil)
	if report.Pass || !slices.Contains(report.Unexplained, path) {
		t.Fatalf("coverage = %#v", report)
	}
}

func snapshotDigests(t *testing.T, repo, state, snapshot string) map[string][32]byte {
	t.Helper()
	files := snapshotFiles(t, repo, state, snapshot)
	digests := make(map[string][32]byte, len(files))
	for path := range files {
		args := []string{"-r", repo, "--password-file", filepath.Join(state, "password"), "dump", snapshot, path}
		data, err := exec.Command("restic", args...).Output()
		if err != nil {
			t.Fatal(err)
		}
		digests[path] = sha256.Sum256(data)
	}
	return digests
}

func TestInterruptedScanOrphanExcludedFromCoverage(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	c, _, state := openTestClient(t, repo)
	line := writeVersion(t, filepath.Join(t.TempDir(), "lost"), "data", "normal")
	stop := errors.New("interrupt")
	c.scanHook = func(point string) error {
		if point == "delta" {
			return stop
		}
		return nil
	}
	if err := c.Scan(t.Context(), inventory.Inventory{Lines: []inventory.Line{line}}, inventory.Delta{Added: []inventory.Line{line}}); err != stop {
		t.Fatalf("scan: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(state, "deltas"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("orphan before open: %v %v", entries, err)
	}
	reopenTestClient(t, state, repo)
	entries, err = os.ReadDir(filepath.Join(state, "deltas"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("orphan after open: %v %v", entries, err)
	}
}
