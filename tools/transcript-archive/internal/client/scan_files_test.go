package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/inventory"
	"github.com/oklog/ulid/v2"
)

func TestScanPersistsAndReplaysNewestDelta(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	c, _, state := openTestClient(t, repo)
	line := inventory.Line{Path: "first", Type: "file", Size: 1}
	if err := c.Scan(t.Context(), inventory.Inventory{Lines: []inventory.Line{line}}, inventory.Delta{Added: []inventory.Line{line}}); err != nil {
		t.Fatal(err)
	}
	first := c.newestDelta
	if err := c.Scan(t.Context(), inventory.Inventory{Lines: []inventory.Line{line}}, inventory.Delta{}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(state, "scan-current.jsonl"), first, c.newestDelta} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o, want 600", path, info.Mode().Perm())
		}
	}
	if filepath.Base(first) >= filepath.Base(c.newestDelta) {
		t.Fatalf("delta sequence did not increase: %s then %s", first, c.newestDelta)
	}
	reopened, err := Open(state, RepoRef{Scope: "k3s-test-one", Repository: repo}, &fakeEscrow{}, WithScratch(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if reopened.newestDelta != c.newestDelta {
		t.Fatalf("replayed delta = %q, want %q", reopened.newestDelta, c.newestDelta)
	}
}

func TestScanDeltaIDsFollowClockAndStayOrdered(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start
	clock := WithClock(func() time.Time { return now })
	repo := filepath.Join(t.TempDir(), "repo")
	c, _, state := openTestClient(t, repo, clock)
	line := inventory.Line{Path: "first", Type: "file", Size: 1}
	var deltas []string
	scan := func(client *Client) {
		t.Helper()
		if err := client.Scan(t.Context(), inventory.Inventory{Lines: []inventory.Line{line}}, inventory.Delta{}); err != nil {
			t.Fatal(err)
		}
		deltas = append(deltas, client.newestDelta)
	}
	passTime := func(delta string) uint64 {
		t.Helper()
		id, err := ulid.Parse(strings.ToUpper(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(delta), "delta-"), ".jsonl")))
		if err != nil {
			t.Fatal(err)
		}
		return id.Time()
	}
	for range 20 {
		scan(c)
	}
	for _, delta := range deltas {
		if got, want := passTime(delta), ulid.Timestamp(start); got != want {
			t.Fatalf("pass time of %s = %d, want injected clock %d", delta, got, want)
		}
	}
	reopened, err := Open(state, RepoRef{Scope: "k3s-test-one", Repository: repo}, &fakeEscrow{}, WithScratch(t.TempDir()), clock)
	if err != nil {
		t.Fatal(err)
	}
	scan(reopened)
	now = start.Add(-time.Minute)
	scan(reopened)
	again, err := Open(state, RepoRef{Scope: "k3s-test-one", Repository: repo}, &fakeEscrow{}, WithScratch(t.TempDir()), clock)
	if err != nil {
		t.Fatal(err)
	}
	scan(again)
	for i := 1; i < len(deltas); i++ {
		if filepath.Base(deltas[i-1]) >= filepath.Base(deltas[i]) {
			t.Fatalf("delta %d = %s does not follow %s", i, deltas[i], deltas[i-1])
		}
	}
}

func TestScanAfterJournalCrashDoesNotReusePassID(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := WithClock(func() time.Time { return start })
	repo := filepath.Join(t.TempDir(), "repo")
	c, _, state := openTestClient(t, repo, clock)
	if err := c.Scan(t.Context(), inventory.Inventory{}, inventory.Delta{}); err != nil {
		t.Fatal(err)
	}
	stop := errors.New("crash at journal")
	c.scanHook = func(point string) error {
		if point == "journal" {
			return stop
		}
		return nil
	}
	a := inventory.Line{Path: "a", Type: "file", Size: 1}
	b := inventory.Line{Path: "b", Type: "file", Size: 7}
	if err := c.Scan(t.Context(), inventory.Inventory{Lines: []inventory.Line{a, b}}, inventory.Delta{Added: []inventory.Line{a, b}}); !errors.Is(err, stop) {
		t.Fatalf("crashed scan: %v", err)
	}
	reopen := func() *Client {
		t.Helper()
		client, err := Open(state, RepoRef{Scope: "k3s-test-one", Repository: repo}, &fakeEscrow{}, WithScratch(t.TempDir()), clock)
		if err != nil {
			t.Fatal(err)
		}
		return client
	}
	if deltas := deltaFiles(t, state); len(deltas) != 2 || !slices.Contains(deltas, c.newestDelta) {
		t.Fatalf("after crash deltas = %v, want the committed delta and one orphan", deltas)
	}
	c = reopen()
	if deltas := deltaFiles(t, state); len(deltas) != 1 || !slices.Contains(deltas, c.newestDelta) {
		t.Fatalf("after first reopen deltas = %v, want only the committed delta", deltas)
	}
	c = reopen()
	if _, bytes := c.Pending(); bytes != 0 {
		t.Fatalf("uncommitted pending after crash: %d", bytes)
	}
	realLine := inventory.Line{Path: "a", Type: "file", Size: 3}
	if err := c.Scan(t.Context(), inventory.Inventory{Lines: []inventory.Line{realLine}}, inventory.Delta{Changed: []inventory.Line{realLine}}); err != nil {
		t.Fatal(err)
	}
	c = reopen()
	if _, bytes := c.Pending(); bytes != realLine.Size {
		t.Fatalf("pending bytes = %d, want %d: crashed scan events were replayed", bytes, realLine.Size)
	}
}

// deltaFiles lists every delta file under the state directory, committed or orphaned.
func deltaFiles(t *testing.T, state string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(state, "deltas", "delta-*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func TestPassRequiresScan(t *testing.T) {
	c, _, _ := openTestClient(t, filepath.Join(t.TempDir(), "repo"))
	if _, err := c.Pass(t.Context(), nil, nil); !errors.Is(err, ErrNoScan) {
		t.Fatalf("pass error = %v, want ErrNoScan", err)
	}
}

func TestPassEmptyListBacksUpDelta(t *testing.T) {
	localRestic(t)
	c, _, _ := openTestClient(t, filepath.Join(t.TempDir(), "repo"))
	if err := c.Scan(t.Context(), inventory.Inventory{}, inventory.Delta{}); err != nil {
		t.Fatal(err)
	}
	outcome, err := c.Pass(t.Context(), nil, nil)
	if err != nil || outcome.Kind != Acked || outcome.SnapshotID == "" {
		t.Fatalf("pass = %#v, %v", outcome, err)
	}
	output, err := c.runJSON(t.Context(), append(c.commonArgs("ls", false), outcome.SnapshotID)...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), filepath.Base(c.newestDelta)) {
		t.Fatalf("snapshot does not contain delta: %s", output)
	}
}

func TestScanPassIdentitySurvivesRestart(t *testing.T) {
	localRestic(t)
	repo := filepath.Join(t.TempDir(), "repo")
	c, _, state := openTestClient(t, repo)
	for range 2 {
		if err := c.Scan(t.Context(), inventory.Inventory{}, inventory.Delta{}); err != nil {
			t.Fatal(err)
		}
	}
	id := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(c.newestDelta), "delta-"), ".jsonl")
	c, err := Open(state, RepoRef{Scope: "k3s-test-one", Repository: repo}, &fakeEscrow{}, WithScratch(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := c.Pass(t.Context(), nil, nil)
	if err != nil || outcome.PassID != id {
		t.Fatalf("pass = %#v, %v; want id %s", outcome, err, id)
	}
	entries, err := os.ReadDir(filepath.Join(state, "deltas"))
	if err != nil || len(entries) != 2 {
		t.Fatalf("deltas = %v, %v", entries, err)
	}
}

func TestOpenRemovesOrphansWithoutHead(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	_, _, state := openTestClient(t, repo)
	if err := os.MkdirAll(filepath.Join(state, "deltas"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"scan-orphan.jsonl", "scan-state-orphan.json", "deltas/delta-orphan.jsonl"} {
		if err := os.WriteFile(filepath.Join(state, name), []byte("orphan"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Open(state, RepoRef{Scope: "k3s-test-one", Repository: repo}, &fakeEscrow{}, WithScratch(t.TempDir())); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"scan-orphan.jsonl", "scan-state-orphan.json", "deltas/delta-orphan.jsonl"} {
		if _, err := os.Stat(filepath.Join(state, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("orphan %s remains: %v", name, err)
		}
	}
}

func TestOpenRebuildsCurrentScan(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	c, _, state := openTestClient(t, repo)
	if err := c.Scan(t.Context(), inventory.Inventory{}, inventory.Delta{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(state, "scan-current.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(state, RepoRef{Scope: "k3s-test-one", Repository: repo}, &fakeEscrow{}, WithScratch(t.TempDir())); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(state, "scan-current.jsonl")); err != nil {
		t.Fatal(err)
	}
}

func TestScanCommitInterruption(t *testing.T) {
	for _, point := range []string{"delta", "journal", "commit"} {
		t.Run(point, func(t *testing.T) {
			checkScanCommitInterruption(t, point)
		})
	}
}

func checkScanCommitInterruption(t *testing.T, point string) {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	c, _, state := openTestClient(t, repo)
	path := filepath.Join(t.TempDir(), "source")
	line := writeVersion(t, path, "data", "normal")
	stop := errors.New("interrupted")
	c.scanHook = func(at string) error {
		if at == point {
			return stop
		}
		return nil
	}
	if err := c.Scan(t.Context(), inventory.Inventory{Lines: []inventory.Line{line}}, inventory.Delta{Added: []inventory.Line{line}}); !errors.Is(err, stop) {
		t.Fatalf("scan: %v", err)
	}
	c = reopenTestClient(t, state, repo)
	if point != "commit" {
		if _, bytes := c.Pending(); bytes != 0 {
			t.Fatalf("uncommitted pending: %d", bytes)
		}
		if err := c.Scan(t.Context(), inventory.Inventory{Lines: []inventory.Line{line}}, inventory.Delta{Added: []inventory.Line{line}}); err != nil {
			t.Fatal(err)
		}
	}
	if oldest, bytes := c.Pending(); oldest.IsZero() || bytes != line.Size {
		t.Fatalf("pending: %v/%d", oldest, bytes)
	}
	id := c.scanID
	outcome := ackFiles(t, c, path)
	if outcome.PassID != id {
		t.Fatalf("pass id %s, want %s", outcome.PassID, id)
	}
}

func TestScanDeltaSummaryCounts(t *testing.T) {
	c, _, _ := openTestClient(t, filepath.Join(t.TempDir(), "repo"))
	line := inventory.Line{Path: "a", Type: "file"}
	delta := inventory.Delta{Added: []inventory.Line{line}, Changed: []inventory.Line{line, line}, Removed: []inventory.Line{line, line, line}}
	if err := c.Scan(t.Context(), inventory.Inventory{}, delta); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(c.newestDelta)
	if err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(strings.TrimSpace(string(data)), "\n")
	var summary inventory.DeltaRow
	if err := json.Unmarshal([]byte(rows[len(rows)-1]), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Event != "summary" || summary.Added != 1 || summary.Changed != 2 || summary.Removed != 3 {
		t.Fatalf("summary: %#v", summary)
	}
}

func TestScanRemovedPendingIsVanished(t *testing.T) {
	c, _, state := openTestClient(t, filepath.Join(t.TempDir(), "repo"))
	line := writeVersion(t, filepath.Join(t.TempDir(), "removed"), "data", "normal")
	scanLines(t, c, line)
	if err := os.Remove(line.Path); err != nil {
		t.Fatal(err)
	}
	if err := c.Scan(t.Context(), inventory.Inventory{}, inventory.Delta{Removed: []inventory.Line{line}}); err != nil {
		t.Fatal(err)
	}
	if !journalHas(t, state, "vanished", line.Path) || journalHas(t, state, "pending-cleared", line.Path) {
		t.Fatal("removed pending path was not journaled vanished")
	}
	if oldest, size := c.Pending(); !oldest.IsZero() || size != 0 {
		t.Fatalf("pending: %v/%d", oldest, size)
	}
}

func TestScanPassScale(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test")
	}
	c, _, state := openTestClient(t, filepath.Join(t.TempDir(), "repo"))
	root := t.TempDir()
	lines := make([]inventory.Line, 10000)
	for i := range lines {
		path := filepath.Join(root, fmt.Sprintf("file-%05d", i))
		lines[i] = writeVersion(t, path, "initial", "normal")
	}
	for i := range 1000 {
		changed := make([]inventory.Line, 3)
		paths := make([]string, 3)
		for j := range changed {
			index := (i*3 + j) % len(lines)
			lines[index] = writeVersion(t, lines[index].Path, fmt.Sprintf("version-%d", i), "normal")
			changed[j], paths[j] = lines[index], lines[index].Path
		}
		if err := c.Scan(t.Context(), inventory.Inventory{Lines: lines}, inventory.Delta{Changed: changed}); err != nil {
			t.Fatal(err)
		}
		if err := c.appendEvent(journalEvent{Event: eventPassStart, PassID: c.scanID, Paths: paths}); err != nil {
			t.Fatal(err)
		}
		if err := c.appendEvent(journalEvent{Event: eventPassAcked, PassID: c.scanID}); err != nil {
			t.Fatal(err)
		}
	}
	assertScaleFiles(t, state)
}

func assertScaleFiles(t *testing.T, state string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(state, "deltas"))
	if err != nil || len(entries) != 1000 {
		t.Fatalf("deltas: %d %v", len(entries), err)
	}
	total := 0
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(state, "deltas", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		total += strings.Count(string(data), "\n")
	}
	if total >= 2*1000*3 {
		t.Fatalf("delta lines: %d", total)
	}
	journal, err := os.Stat(filepath.Join(state, "journal.jsonl"))
	if err != nil || journal.Size() > 6_000_000 {
		t.Fatalf("journal: %v %v", journal, err)
	}
	if _, err := os.Stat(filepath.Join(state, "scan-current.jsonl")); err != nil {
		t.Fatal(err)
	}
}
