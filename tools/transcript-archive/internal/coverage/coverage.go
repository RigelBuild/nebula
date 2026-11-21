package coverage

import (
	"bufio"
	"encoding/json"
	"io"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/inventory"
)

// FileDigest holds the complete file hash and digests for recorded prefixes.
type FileDigest struct {
	SHA256   [32]byte           `json:"sha256"`
	Prefixes map[int64][32]byte `json:"prefixes"`
}

// HistoryEntry identifies a prior snapshot that holds an earlier file version.
type HistoryEntry struct {
	SnapshotID string   `json:"snapshotId"`
	Path       string   `json:"path"`
	Size       int64    `json:"size"`
	SHA256     [32]byte `json:"sha256"`
}

// SnapshotListing lists the paths and SHA-256 values present in one snapshot.
type SnapshotListing struct {
	SnapshotID string              `json:"snapshotId"`
	PassID     string              `json:"passId,omitempty"`
	Outcome    string              `json:"outcome"`
	PathsOnly  bool                `json:"pathsOnly,omitempty"`
	Files      map[string][32]byte `json:"files"`
	// StateFiles are the archive's own state files (deltas, journal) that the snapshot may hold.
	StateFiles map[string]struct{} `json:"-"`
}

// Report contains the results of forward, backward and loss checks.
type Report struct {
	Missing      []string       `json:"missing"`
	Extra        []string       `json:"extra"`
	Unexplained  []string       `json:"unexplained"`
	Truncated    []string       `json:"truncated"`
	Rewritten    []string       `json:"rewritten"`
	HashMismatch []string       `json:"hashMismatch"`
	History      []HistoryEntry `json:"history"`
	Vanished     int            `json:"vanished"`
	Pass         bool           `json:"pass"`
}

type versionKey struct {
	path   string
	digest [32]byte
}

type versions struct {
	current  map[string]inventory.Line
	needed   map[versionKey]inventory.Line
	held     map[versionKey]int
	final    map[string]inventory.Line
	baseline map[string]inventory.Line
	snap     SnapshotListing
	digests  map[string]FileDigest
	chained  bool
}

// Delta identifies the pass associated with a streamed scan delta.
type Delta struct {
	PassID string
	R      io.Reader
}

// Check compares inventories, streamed scan deltas and snapshot contents.
func Check(final, baseline inventory.Inventory, deltas []Delta, chained bool, snap SnapshotListing, digests map[string]FileDigest, history []SnapshotListing, vanished []string) Report {
	report := Report{
		Missing: []string{}, Extra: []string{}, Unexplained: []string{},
		Truncated: []string{}, Rewritten: []string{}, HashMismatch: []string{},
		History: []HistoryEntry{},
	}
	vanishedSet := make(map[string]struct{}, len(vanished))
	for _, path := range vanished {
		vanishedSet[path] = struct{}{}
	}
	report.Vanished = len(vanishedSet)

	snap.Files = cleanSnapshotPaths(snap.Files)
	history = slices.Clone(history)
	for i := range history {
		history[i].Files = cleanSnapshotPaths(history[i].Files)
	}
	finalFiles := fileMap(final)
	baselineFiles := fileMap(baseline)
	checkSnapshot(&report, finalFiles, baselineFiles, chained, snap, digests)

	state := versions{current: fileMap(baseline), needed: make(map[versionKey]inventory.Line), held: make(map[versionKey]int), final: finalFiles, baseline: baselineFiles, snap: snap, digests: digests, chained: chained}
	for _, line := range baseline.Lines {
		state.remember(line)
	}
	for _, delta := range deltas {
		if delta.PassID == "" || delta.R == nil {
			appendUnique(&report.Unexplained, "invalid-delta")
			continue
		}
		if err := replayDelta(delta.R, &state); err != nil {
			appendUnique(&report.Unexplained, "invalid-delta")
			continue
		}
		state.recordPass(delta.PassID, history)
	}
	state.checkNeeded(&report, history, vanishedSet)
	for _, item := range final.Errors {
		appendUnique(&report.Unexplained, item.Path)
	}
	for _, item := range baseline.Errors {
		appendUnique(&report.Unexplained, item.Path)
	}
	sortReport(&report)
	report.Pass = reportIsEmpty(report)
	return report
}

func (state *versions) checkNeeded(report *Report, history []SnapshotListing, vanished map[string]struct{}) {
	deletedHeld := make(map[string]versionKey)
	for key, index := range state.held {
		if _, present := state.final[key.path]; present {
			continue
		}
		old, exists := deletedHeld[key.path]
		if !exists || index < state.held[old] {
			deletedHeld[key.path] = key
		}
	}
	for key, line := range state.needed {
		if credited, ok := deletedHeld[key.path]; ok {
			if key == credited {
				appendHistory(report, HistoryEntry{SnapshotID: history[state.held[key]].SnapshotID, Path: key.path, Size: line.Size, SHA256: key.digest})
			}
			continue
		}
		if err := checkVersion(line, report, state.final, state.baseline, state.chained, state.snap, state.digests, history, vanished, state.held, key); err != nil {
			appendUnique(&report.Unexplained, "invalid-delta")
		}
	}
}

func checkSnapshot(report *Report, finalFiles, baselineFiles map[string]inventory.Line, chained bool, snap SnapshotListing, digests map[string]FileDigest) {
	for path, file := range finalFiles {
		if file.Class != "normal" || pathInState(snap, path) {
			continue
		}
		old, existed := baselineFiles[path]
		if chained && existed && sameLine(old, file) {
			continue
		}
		archived, ok := snap.Files[path]
		if !ok {
			appendUnique(&report.Missing, path)
		} else if archived != file.SHA256 {
			appendUnique(&report.HashMismatch, path)
		}
		if digest, ok := digests[path]; ok && archived != digest.SHA256 {
			appendUnique(&report.HashMismatch, path)
		}
	}
	for path := range snap.Files {
		if _, ok := finalFiles[path]; !ok && !pathInState(snap, path) && !isMaterializedPath(path) && !isDeletedBaseline(path, baselineFiles) {
			appendUnique(&report.Extra, path)
		}
	}
}

func replayDelta(reader io.Reader, state *versions) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var row inventory.DeltaRow
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return err
		}
		switch row.Event {
		case "summary":
			if row.Line != nil {
				return io.ErrUnexpectedEOF
			}
		case "added", "changed", "removed":
			if row.Line == nil || row.Line.Path == "" {
				return io.ErrUnexpectedEOF
			}
			if row.Event == "removed" {
				delete(state.current, row.Line.Path)
				continue
			}
			state.current[row.Line.Path] = *row.Line
			state.remember(*row.Line)
		default:
			return io.ErrUnexpectedEOF
		}
	}
	return scanner.Err()
}

func (state *versions) remember(line inventory.Line) {
	if line.Class != "normal" {
		return
	}
	if archived, ok := state.snap.Files[line.Path]; ok && archived == line.SHA256 {
		return
	}
	if final, ok := state.final[line.Path]; ok && (final.SHA256 == line.SHA256 || extends(final, line, state.digests[line.Path])) {
		return
	}
	if base, ok := state.baseline[line.Path]; state.chained && ok && sameLine(line, base) {
		return
	}
	state.needed[versionKey{line.Path, line.SHA256}] = line
}

func (state *versions) recordPass(pass string, history []SnapshotListing) {
	for index, listing := range history {
		if listing.PassID != pass || !listing.PathsOnly || (listing.Outcome != "pass-acked" && listing.Outcome != "pass-incomplete" && listing.Outcome != "pass-stored") {
			continue
		}
		for path := range listing.Files {
			line, ok := state.current[path]
			if !ok {
				continue
			}
			key := versionKey{path, line.SHA256}
			if _, needed := state.needed[key]; !needed {
				continue
			}
			if old, exists := state.held[key]; !exists || index < old {
				state.held[key] = index
			}
		}
	}
}

func checkVersion(line inventory.Line, report *Report, final map[string]inventory.Line, baseline map[string]inventory.Line, chained bool, snap SnapshotListing, digests map[string]FileDigest, history []SnapshotListing, vanished map[string]struct{}, held map[versionKey]int, key versionKey) error {
	if line.Path == "" || line.Class == "final-only" {
		return nil
	}
	if line.Path == "invalid-delta" {
		return io.ErrUnexpectedEOF
	}
	if archived, ok := snap.Files[line.Path]; ok && archived == line.SHA256 {
		return nil
	}
	current, exists := final[line.Path]
	if exists && (current.SHA256 == line.SHA256 || extends(current, line, digests[line.Path])) {
		return nil
	}
	if old, ok := baseline[line.Path]; chained && ok && sameLine(line, old) {
		return nil
	}
	if index, ok := held[key]; ok {
		appendHistory(report, HistoryEntry{SnapshotID: history[index].SnapshotID, Path: line.Path, Size: line.Size, SHA256: line.SHA256})
		return nil
	}
	if _, gone := vanished[line.Path]; gone {
		return nil
	}
	if !exists {
		appendUnique(&report.Unexplained, line.Path)
		return nil
	}
	if current.Size < line.Size {
		appendUnique(&report.Truncated, line.Path)
		return nil
	}
	appendUnique(&report.Rewritten, line.Path)
	return nil
}

func extends(current, earlier inventory.Line, digest FileDigest) bool {
	if current.Size < earlier.Size {
		return false
	}
	if current.SHA256 == earlier.SHA256 {
		return true
	}
	prefix, ok := digest.Prefixes[earlier.Size]
	return ok && prefix == earlier.SHA256
}

func appendHistory(report *Report, entry HistoryEntry) {
	for index := range report.History {
		if report.History[index].Path == entry.Path && report.History[index].SHA256 == entry.SHA256 {
			report.History[index] = entry
			return
		}
	}
	report.History = append(report.History, entry)
}

func fileMap(inv inventory.Inventory) map[string]inventory.Line {
	files := make(map[string]inventory.Line, len(inv.Lines))
	for _, line := range inv.Lines {
		if line.Type != "directory" && line.Type != "other" {
			files[line.Path] = line
		}
	}
	return files
}

func sameLine(left, right inventory.Line) bool {
	return left.Path == right.Path && left.Type == right.Type && left.Size == right.Size && left.SHA256 == right.SHA256
}

func appendUnique(paths *[]string, path string) {
	if slices.Contains(*paths, path) {
		return
	}
	*paths = append(*paths, path)
}

func sortReport(report *Report) {
	sort.Strings(report.Missing)
	sort.Strings(report.Extra)
	sort.Strings(report.Unexplained)
	sort.Strings(report.Truncated)
	sort.Strings(report.Rewritten)
	sort.Strings(report.HashMismatch)
	sort.Slice(report.History, func(i, j int) bool {
		if report.History[i].Path != report.History[j].Path {
			return report.History[i].Path < report.History[j].Path
		}
		return report.History[i].SnapshotID < report.History[j].SnapshotID
	})
}

func reportIsEmpty(report Report) bool {
	return len(report.Missing) == 0 && len(report.Extra) == 0 && len(report.Unexplained) == 0 &&
		len(report.Truncated) == 0 && len(report.Rewritten) == 0 && len(report.HashMismatch) == 0
}

func cleanSnapshotPaths(files map[string][32]byte) map[string][32]byte {
	cleaned := make(map[string][32]byte, len(files))
	for path, digest := range files {
		cleaned[filepath.ToSlash(filepath.Clean(path))] = digest
	}
	return cleaned
}

// pathInState reports whether path is one of the state files coverage was given; only those are exempt.
func pathInState(snap SnapshotListing, path string) bool {
	_, ok := snap.StateFiles[filepath.ToSlash(filepath.Clean(path))]
	return ok
}

func isMaterializedPath(path string) bool {
	base := path[strings.LastIndexByte(path, '/')+1:]
	return base == "config.yml" || base == "models.yml" || base == "gc.lock" || strings.HasSuffix(base, ".pid") || strings.Contains(base, ".pid.")
}

func isDeletedBaseline(path string, baseline map[string]inventory.Line) bool {
	_, exists := baseline[path]
	return exists
}
