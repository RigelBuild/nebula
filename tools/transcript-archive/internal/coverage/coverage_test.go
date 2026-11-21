package coverage

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/inventory"
)

func TestCheckHistoryCandidates(t *testing.T) {
	old := invLine("/file", 3, sha256.Sum256([]byte("old")))
	changed := invLine("/file", 3, sha256.Sum256([]byte("new")))
	history := []SnapshotListing{{SnapshotID: "newest", PassID: "one", Outcome: "pass-stored", PathsOnly: true, Files: map[string][32]byte{"/file": {}}}, {SnapshotID: "older", PassID: "one", Outcome: "pass-acked", PathsOnly: true, Files: map[string][32]byte{"/file": {}}}}
	report := Check(inventory.Inventory{Lines: []inventory.Line{changed}}, inventory.Inventory{}, []Delta{{PassID: "one", R: strings.NewReader(lineJSON(t, old))}}, false, SnapshotListing{Files: map[string][32]byte{"/file": changed.SHA256}}, nil, history, nil)
	if !report.Pass || len(report.History) != 1 || report.History[0].SnapshotID != "newest" || report.History[0].SHA256 != old.SHA256 || report.History[0].Size != old.Size {
		t.Fatalf("history candidates: %#v", report)
	}
}

func TestCheckDeltaIdentity(t *testing.T) {
	line := invLine("/file", 3, sha256.Sum256([]byte("old")))
	history := []SnapshotListing{{SnapshotID: "s1", PassID: "one", Outcome: "pass-acked", PathsOnly: true, Files: map[string][32]byte{line.Path: {}}}}
	report := Check(inventory.Inventory{}, inventory.Inventory{}, []Delta{{PassID: "one", R: strings.NewReader(lineJSON(t, line))}}, false, SnapshotListing{}, nil, history, nil)
	if !report.Pass || len(report.History) != 1 {
		t.Fatalf("explicit pass identity: %#v", report)
	}
	report = Check(inventory.Inventory{}, inventory.Inventory{}, []Delta{{R: strings.NewReader(lineJSON(t, line))}}, false, SnapshotListing{}, nil, nil, nil)
	if !slices.Contains(report.Unexplained, "invalid-delta") {
		t.Fatalf("missing pass identity accepted: %#v", report)
	}
}

func TestCheckDeletedAfterFailedPass(t *testing.T) {
	first := invLine("/file", 3, sha256.Sum256([]byte("one")))
	second := invLine("/file", 3, sha256.Sum256([]byte("two")))
	history := []SnapshotListing{{SnapshotID: "s2", PassID: "two", Outcome: "pass-acked", PathsOnly: true, Files: map[string][32]byte{"/file": {}}}}
	removed, err := json.Marshal(inventory.DeltaRow{Event: "removed", Line: &second})
	if err != nil {
		t.Fatal(err)
	}
	deltas := []Delta{{PassID: "one", R: strings.NewReader(lineJSON(t, first))}, {PassID: "two", R: strings.NewReader(lineJSON(t, second))}, {PassID: "three", R: strings.NewReader(string(removed))}}
	report := Check(inventory.Inventory{}, inventory.Inventory{}, deltas, false, SnapshotListing{}, nil, history, nil)
	if !report.Pass || len(report.History) != 1 || report.History[0].SnapshotID != "s2" || report.History[0].SHA256 != second.SHA256 {
		t.Fatalf("deleted path history: %#v", report)
	}
}

func TestCheckPresentStillRequiresEveryVersion(t *testing.T) {
	first := invLine("/file", 3, sha256.Sum256([]byte("one")))
	second := invLine("/file", 3, sha256.Sum256([]byte("two")))
	final := invLine("/file", 5, sha256.Sum256([]byte("three")))
	history := []SnapshotListing{{SnapshotID: "s2", PassID: "two", Outcome: "pass-acked", PathsOnly: true, Files: map[string][32]byte{first.Path: {}}}}
	deltas := []Delta{{PassID: "one", R: strings.NewReader(lineJSON(t, first))}, {PassID: "two", R: strings.NewReader(lineJSON(t, second))}}
	report := Check(inventory.Inventory{Lines: []inventory.Line{final}}, inventory.Inventory{}, deltas, false, SnapshotListing{Files: map[string][32]byte{final.Path: final.SHA256}}, nil, history, nil)
	if report.Pass || !slices.Contains(report.Rewritten, first.Path) {
		t.Fatalf("uncredited present version accepted: %#v", report)
	}
}

func TestCheckHistoryExactReplay(t *testing.T) {
	first := invLine("/file", 3, sha256.Sum256([]byte("one")))
	second := invLine("/file", 3, sha256.Sum256([]byte("two")))
	history := []SnapshotListing{
		{SnapshotID: "s2", PassID: "two", Outcome: "pass-acked", PathsOnly: true, Files: map[string][32]byte{"/file": {}}},
		{SnapshotID: "s1", PassID: "one", Outcome: "pass-acked", PathsOnly: true, Files: map[string][32]byte{"/file": {}}},
	}
	deltas := []Delta{{PassID: "one", R: strings.NewReader(lineJSON(t, first))}, {PassID: "two", R: strings.NewReader(lineJSON(t, second))}}
	report := Check(inventory.Inventory{}, inventory.Inventory{}, deltas, false, SnapshotListing{}, nil, history, nil)
	if !report.Pass || len(report.History) != 1 || report.History[0].SHA256 != second.SHA256 || report.History[0].SnapshotID != "s2" {
		t.Fatalf("history: %#v", report)
	}
}

func TestCheckDoesNotMutateHistory(t *testing.T) {
	history := []SnapshotListing{{Files: map[string][32]byte{"/a/../file": {1}}}}
	Check(inventory.Inventory{}, inventory.Inventory{}, nil, false, SnapshotListing{}, nil, history, nil)
	if _, ok := history[0].Files["/a/../file"]; !ok {
		t.Fatalf("history mutated: %#v", history)
	}
}

func TestCheckAbsoluteSnapshotPathsAndBookkeeping(t *testing.T) {
	line := invLine("/seat/data/file", 4, sha256.Sum256([]byte("data")))
	state := "/state/deltas/1.jsonl"
	files := map[string][32]byte{
		"/seat/data/../data/file": line.SHA256,
		state:                     {},
	}
	report := Check(inventory.Inventory{Lines: []inventory.Line{line}}, inventory.Inventory{}, nil, false, SnapshotListing{Files: files, StateFiles: map[string]struct{}{state: {}}}, nil, nil, nil)
	if !report.Pass {
		t.Fatalf("coverage = %#v", report)
	}
}

// A transcript under a nested .example/archive is ordinary data: missing from the snapshot fails coverage.
func TestCheckNestedArchiveIsNotExempt(t *testing.T) {
	line := invLine("/workspace/u/.example/agents/seat/.example/archive/id/session.jsonl", 4, sha256.Sum256([]byte("data")))
	report := Check(inventory.Inventory{Lines: []inventory.Line{line}}, inventory.Inventory{}, nil, false, SnapshotListing{Files: map[string][32]byte{}}, nil, nil, nil)
	if report.Pass || !slices.Contains(report.Missing, line.Path) {
		t.Fatalf("nested archive transcript exempted: %#v", report)
	}
}

func TestCheckClientDeltaLostVersion(t *testing.T) {
	line := invLine("lost", 4, sha256.Sum256([]byte("lost")))
	row, err := json.Marshal(struct {
		Event string         `json:"event"`
		Line  inventory.Line `json:"line"`
	}{Event: "added", Line: line})
	if err != nil {
		t.Fatal(err)
	}
	report := Check(inventory.Inventory{}, inventory.Inventory{}, []Delta{{PassID: "test", R: strings.NewReader(string(row))}}, false, SnapshotListing{}, nil, nil, nil)
	if !slices.Contains(report.Unexplained, line.Path) {
		t.Fatalf("lost delta version was not reported: %#v", report)
	}
}

func TestCheckRejectsUnknownDeltaRow(t *testing.T) {
	report := Check(inventory.Inventory{}, inventory.Inventory{}, []Delta{{PassID: "test", R: strings.NewReader(`{"event":"unknown"}`)}}, false, SnapshotListing{}, nil, nil, nil)
	if !slices.Contains(report.Unexplained, "invalid-delta") {
		t.Fatalf("unknown delta row was accepted: %#v", report)
	}
}

func TestCheckDeletedBaselineLoss(t *testing.T) {
	base := invLine("lost", 4, sha256.Sum256([]byte("base")))
	report := Check(inventory.Inventory{}, inventory.Inventory{Lines: []inventory.Line{base}}, nil, false, SnapshotListing{}, nil, nil, nil)
	if !slices.Contains(report.Unexplained, base.Path) {
		t.Fatalf("deleted baseline was not reported: %#v", report)
	}
}

func TestCheckChangedBaselineLoss(t *testing.T) {
	base := invLine("file", 8, sha256.Sum256([]byte("original")))
	for _, tc := range []struct {
		name string
		size int64
	}{{"truncated", 2}, {"rewritten", 8}} {
		t.Run(tc.name, func(t *testing.T) {
			final := invLine("file", tc.size, sha256.Sum256([]byte(tc.name)))
			report := Check(inventory.Inventory{Lines: []inventory.Line{final}}, inventory.Inventory{Lines: []inventory.Line{base}}, nil, false, SnapshotListing{Files: map[string][32]byte{"file": final.SHA256}}, nil, nil, nil)
			if tc.name == "truncated" && !slices.Contains(report.Truncated, "file") || tc.name == "rewritten" && !slices.Contains(report.Rewritten, "file") {
				t.Fatalf("loss not classified: %#v", report)
			}
		})
	}
}

func TestCheckEqualHashVersion(t *testing.T) {
	line := invLine("file", 4, sha256.Sum256([]byte("same")))
	report := Check(inventory.Inventory{Lines: []inventory.Line{line}}, inventory.Inventory{}, []Delta{{PassID: "test", R: strings.NewReader(lineJSON(t, line))}}, false, SnapshotListing{Files: map[string][32]byte{line.Path: line.SHA256}}, nil, nil, nil)
	if !report.Pass {
		t.Fatalf("equal final hash was rejected: %#v", report)
	}
}

func TestCheckGrowingFile(t *testing.T) {
	t.Parallel()
	first := sha256.Sum256([]byte("first"))
	secondBytes := []byte("first and longer")
	second := sha256.Sum256(secondBytes)
	prefix := sha256.Sum256([]byte("first"))
	baseline := invLine("sessions/a.jsonl", 5, first)
	final := invLine("sessions/a.jsonl", int64(len(secondBytes)), second)
	report := Check(
		inventory.Inventory{Lines: []inventory.Line{final}},
		inventory.Inventory{Lines: []inventory.Line{baseline}},
		[]Delta{{PassID: "test", R: strings.NewReader(lineJSON(t, final))}},
		false,
		SnapshotListing{Files: map[string][32]byte{final.Path: second}},
		map[string]FileDigest{final.Path: {SHA256: second, Prefixes: map[int64][32]byte{5: prefix}}},
		nil,
		nil,
	)
	if !report.Pass {
		t.Fatalf("growing append-only file should pass: %#v", report)
	}
}

func TestCheckFailureClassification(t *testing.T) {
	t.Parallel()
	old := sha256.Sum256([]byte("original"))
	oldLine := invLine("sessions/a.jsonl", 8, old)
	cases := []struct {
		name       string
		final      []inventory.Line
		delta      []inventory.Line
		digests    map[string]FileDigest
		snapshot   SnapshotListing
		vanished   []string
		wantBucket string
	}{
		{
			name:       "truncated",
			final:      []inventory.Line{invLine(oldLine.Path, 4, sha256.Sum256([]byte("less")))},
			delta:      []inventory.Line{oldLine},
			wantBucket: "truncated",
		},
		{
			name:       "rewritten",
			final:      []inventory.Line{invLine(oldLine.Path, 8, sha256.Sum256([]byte("rewritten")))},
			delta:      []inventory.Line{oldLine},
			wantBucket: "rewritten",
		},
		{
			name:       "unexplained missing scan version",
			final:      []inventory.Line{},
			delta:      []inventory.Line{oldLine},
			wantBucket: "unexplained",
		},
		{
			name:       "vanished",
			final:      []inventory.Line{},
			delta:      []inventory.Line{oldLine},
			vanished:   []string{oldLine.Path},
			wantBucket: "pass",
		},
		{
			name:       "snapshot digest mismatch",
			final:      []inventory.Line{oldLine},
			delta:      []inventory.Line{oldLine},
			snapshot:   SnapshotListing{Files: map[string][32]byte{oldLine.Path: sha256.Sum256([]byte("wrong"))}},
			wantBucket: "hashMismatch",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var deltaReader io.Reader
			if len(test.delta) > 0 {
				deltaReader = strings.NewReader(lineJSON(t, test.delta[0]))
			}
			report := Check(inventory.Inventory{Lines: test.final}, inventory.Inventory{Lines: []inventory.Line{oldLine}}, []Delta{{PassID: "test", R: deltaReader}}, false, test.snapshot, test.digests, nil, test.vanished)
			assertFailureBucket(t, report, test.wantBucket, oldLine.Path)
		})
	}
}

func assertFailureBucket(t *testing.T, report Report, bucket, path string) {
	t.Helper()
	if bucket == "pass" {
		if !report.Pass || report.Vanished != 1 {
			t.Fatalf("vanished report = %#v", report)
		}
		return
	}
	if report.Pass {
		t.Fatalf("expected %s failure: %#v", bucket, report)
	}
	if !bucketHas(report, bucket, path) {
		t.Fatalf("%s missing from report: %#v", bucket, report)
	}
}

func TestCheckBackwardAndHeldHistory(t *testing.T) {
	t.Parallel()
	deleted := invLine("sessions/deleted.jsonl", 4, sha256.Sum256([]byte("gone")))
	finalLine := invLine("sessions/final.jsonl", 5, sha256.Sum256([]byte("final")))
	report := Check(
		inventory.Inventory{Lines: []inventory.Line{finalLine}},
		inventory.Inventory{Lines: []inventory.Line{deleted}},
		[]Delta{{PassID: "test", R: strings.NewReader(lineJSON(t, deleted))}},
		false,
		SnapshotListing{Files: map[string][32]byte{finalLine.Path: finalLine.SHA256}},
		map[string]FileDigest{finalLine.Path: {SHA256: finalLine.SHA256}},
		[]SnapshotListing{{SnapshotID: "stored-id", PassID: "test", Outcome: "pass-stored", PathsOnly: true, Files: map[string][32]byte{deleted.Path: {}}}},
		nil,
	)
	if !report.Pass || len(report.History) != 1 || report.History[0].SnapshotID != "stored-id" {
		t.Fatalf("stored snapshot history = %#v", report)
	}

	extra := invLine("stale", 1, sha256.Sum256([]byte("x")))
	report = Check(
		inventory.Inventory{Lines: []inventory.Line{finalLine}}, inventory.Inventory{}, nil, false,
		SnapshotListing{Files: map[string][32]byte{extra.Path: extra.SHA256}},
		map[string]FileDigest{extra.Path: {SHA256: extra.SHA256}}, nil, nil,
	)
	if report.Pass || len(report.Extra) != 1 || len(report.Missing) != 1 {
		t.Fatalf("backward/forward report = %#v", report)
	}
}

func TestCheckSeatDelta(t *testing.T) {
	t.Parallel()
	baselineLines := make([]inventory.Line, 100)
	finalLines := make([]inventory.Line, 100, 103)
	files := make(map[string][32]byte, 5)
	for index := range baselineLines {
		path := fmt.Sprintf("file-%03d", index)
		digest := sha256.Sum256([]byte(fmt.Sprintf("baseline-%d", index)))
		baselineLines[index] = invLine(path, 10, digest)
		finalLines[index] = baselineLines[index]
	}
	for index := range 3 {
		line := invLine(fmt.Sprintf("added-%d", index), 10, sha256.Sum256([]byte(fmt.Sprintf("added-%d", index))))
		finalLines = append(finalLines, line)
		files[line.Path] = line.SHA256
	}
	for index := range 2 {
		path := fmt.Sprintf("file-%03d", index)
		line := invLine(path, 11, sha256.Sum256([]byte(fmt.Sprintf("changed-%d", index))))
		finalLines[index] = line
		files[line.Path] = line.SHA256
	}
	deltaLines := append([]inventory.Line(nil), finalLines[100:]...)
	deltaLines = append(deltaLines, finalLines[:2]...)
	reader := strings.NewReader(strings.Join(marshalLines(t, deltaLines), "\n"))
	report := Check(inventory.Inventory{Lines: finalLines}, inventory.Inventory{Lines: baselineLines}, []Delta{{PassID: "test", R: reader}}, true, SnapshotListing{Files: files}, digestMap(finalLines), nil, nil)
	if !report.Pass || len(files) != 5 {
		t.Fatalf("seat delta checked %d files: %#v", len(files), report)
	}
	missingFiles := maps.Clone(files)
	delete(missingFiles, finalLines[0].Path)
	report = Check(inventory.Inventory{Lines: finalLines}, inventory.Inventory{Lines: baselineLines}, nil, true, SnapshotListing{Files: missingFiles}, digestMap(finalLines), nil, nil)
	if !slices.Contains(report.Missing, finalLines[0].Path) {
		t.Fatalf("missing rewritten file not reported: %#v", report)
	}

	finalWithout := append([]inventory.Line(nil), finalLines[:99]...)
	finalWithout = append(finalWithout, finalLines[99:]...)
	finalWithout = append(finalWithout[:99], finalWithout[100:]...)
	report = Check(inventory.Inventory{Lines: finalWithout}, inventory.Inventory{Lines: baselineLines}, nil, true, SnapshotListing{Files: files}, digestMap(finalWithout), nil, nil)
	if !report.Pass {
		t.Fatalf("unchanged baseline deletion should pass: %#v", report)
	}
}

func TestCheckChainedRevert(t *testing.T) {
	t.Parallel()
	base := invLine("file", 4, sha256.Sum256([]byte("base")))
	changed := invLine("file", 7, sha256.Sum256([]byte("changed")))
	report := Check(
		inventory.Inventory{Lines: []inventory.Line{base}},
		inventory.Inventory{Lines: []inventory.Line{base}},
		[]Delta{{PassID: "test", R: strings.NewReader(lineJSON(t, changed))}},
		true,
		SnapshotListing{Files: map[string][32]byte{}},
		map[string]FileDigest{}, nil, nil,
	)
	if !slices.Contains(report.Truncated, changed.Path) {
		t.Fatalf("lost changed version was not reported: %#v", report)
	}
}

func TestCheckScanDeltaMemoryBound(t *testing.T) {
	t.Parallel()
	const fileCount = 10000
	const passCount = 1000
	baselineLines := make([]inventory.Line, fileCount)
	finalLines := make([]inventory.Line, fileCount)
	for index := range baselineLines {
		line := invLine(fmt.Sprintf("file-%05d", index), 10, sha256.Sum256([]byte(fmt.Sprintf("v0-%d", index))))
		baselineLines[index] = line
		finalLines[index] = line
	}
	readers := make([]Delta, passCount)
	for pass := range passCount {
		lines := make([]inventory.Line, 3)
		for changed := range lines {
			index := (pass*3 + changed) % fileCount
			line := invLine(baselineLines[index].Path, 11, sha256.Sum256([]byte(fmt.Sprintf("pass-%d-file-%d", pass, index))))
			lines[changed] = line
			finalLines[index] = line
		}
		readers[pass] = Delta{PassID: strconv.Itoa(pass), R: strings.NewReader(strings.Join(marshalLines(t, lines), "\n"))}
	}
	files := make(map[string][32]byte, fileCount)
	for _, line := range finalLines {
		files[line.Path] = line.SHA256
	}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	report := Check(inventory.Inventory{Lines: finalLines}, inventory.Inventory{Lines: baselineLines}, readers, true, SnapshotListing{Files: files}, digestMap(finalLines), nil, nil)
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if !report.Pass {
		t.Fatalf("scan replay failed: %#v", report)
	}
	if after.TotalAlloc-before.TotalAlloc > 256*1024*1024 {
		t.Fatalf("scan replay allocated %d bytes, exceeds one-walk-set budget", after.TotalAlloc-before.TotalAlloc)
	}
}

func invLine(path string, size int64, digest [32]byte) inventory.Line {
	return inventory.Line{Root: "root", Path: path, Type: "file", Record: "tree/root", Class: "normal", Size: size, SHA256: digest}
}

func lineJSON(t *testing.T, line inventory.Line) string {
	t.Helper()
	encoded, err := json.Marshal(inventory.DeltaRow{Event: "changed", Line: &line})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func marshalLines(t *testing.T, lines []inventory.Line) []string {
	t.Helper()
	encoded := make([]string, len(lines))
	for index, line := range lines {
		encoded[index] = lineJSON(t, line)
	}
	return encoded
}

func digestMap(lines []inventory.Line) map[string]FileDigest {
	digests := make(map[string]FileDigest, len(lines))
	for _, line := range lines {
		digests[line.Path] = FileDigest{SHA256: line.SHA256}
	}
	return digests
}

func bucketHas(report Report, bucket, path string) bool {
	var paths []string
	switch bucket {
	case "missing":
		paths = report.Missing
	case "extra":
		paths = report.Extra
	case "unexplained":
		paths = report.Unexplained
	case "truncated":
		paths = report.Truncated
	case "rewritten":
		paths = report.Rewritten
	case "hashMismatch":
		paths = report.HashMismatch
	}
	return slices.Contains(paths, path)
}
