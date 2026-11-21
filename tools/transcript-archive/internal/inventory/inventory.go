package inventory

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"sort"
)

// Line describes one file in a source walk.
type Line struct {
	Root       string   `json:"root"`
	Path       string   `json:"path"`
	Type       string   `json:"type"`
	Record     string   `json:"record"`
	SessionID  string   `json:"sessionId,omitempty"`
	Class      string   `json:"class"`
	LinkTarget string   `json:"linkTarget,omitempty"`
	Size       int64    `json:"size"`
	MtimeNs    int64    `json:"mtimeNs"`
	SHA256     [32]byte `json:"sha256"`
}

// Dangling records a blob or artifact reference without valid closure.
type Dangling struct {
	Path      string `json:"path"`
	Record    string `json:"record"`
	Reference string `json:"reference"`
	Reason    string `json:"reason"`
}

// Excluded reports aggregate size and count without storing excluded content.
type Excluded struct {
	Count int   `json:"count"`
	Bytes int64 `json:"bytes"`
}

// AssignError describes a failed record assignment or an unclassified watch file.
type AssignError struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Inventory is a complete result for one walk.
type Inventory struct {
	Lines          []Line        `json:"lines"`
	Dangling       []Dangling    `json:"dangling"`
	Excluded       []Excluded    `json:"excluded"`
	OutsideTargets int           `json:"outsideTargets"`
	Errors         []AssignError `json:"errors"`
}

// Delta contains the file lines added, changed or removed between walks.
type Delta struct {
	Added   []Line `json:"added"`
	Changed []Line `json:"changed"`
	Removed []Line `json:"removed"`
}

// DeltaRow is one change or the counts summary in a scan delta.
type DeltaRow struct {
	Event   string `json:"event"`
	Line    *Line  `json:"line,omitempty"`
	Added   int    `json:"added,omitempty"`
	Changed int    `json:"changed,omitempty"`
	Removed int    `json:"removed,omitempty"`
}

// WriteDelta encodes a delta in the shared JSONL format.
func WriteDelta(enc *json.Encoder, delta Delta) error {
	for _, group := range []struct {
		event string
		lines []Line
	}{{"added", delta.Added}, {"changed", delta.Changed}, {"removed", delta.Removed}} {
		for _, line := range group.lines {
			if err := enc.Encode(DeltaRow{Event: group.event, Line: &line}); err != nil {
				return err
			}
		}
	}
	return enc.Encode(DeltaRow{Event: "summary", Added: len(delta.Added), Changed: len(delta.Changed), Removed: len(delta.Removed)})
}

// Since returns the current file versions that differ from the baseline.
// MtimeNs is deliberately excluded from the comparison.
func Since(baseline, now Inventory) []Line {
	previous := make(map[string]Line, len(baseline.Lines))
	for _, line := range baseline.Lines {
		previous[lineKey(line)] = line
	}

	changed := make([]Line, 0)
	for _, line := range now.Lines {
		old, ok := previous[lineKey(line)]
		if !ok || !sameContent(old, line) {
			changed = append(changed, line)
		}
	}
	sortLines(changed)
	return changed
}

// ContentDigest hashes the sorted file path, type, size and SHA-256 tuples.
// Mtime, exclusions, errors and summary data do not affect the digest.
func ContentDigest(inv Inventory) [32]byte {
	lines := append([]Line(nil), inv.Lines...)
	sort.Slice(lines, func(i, j int) bool {
		if lines[i].Path != lines[j].Path {
			return lines[i].Path < lines[j].Path
		}
		if lines[i].Type != lines[j].Type {
			return lines[i].Type < lines[j].Type
		}
		if lines[i].Size != lines[j].Size {
			return lines[i].Size < lines[j].Size
		}
		return string(lines[i].SHA256[:]) < string(lines[j].SHA256[:])
	})

	hash := sha256.New()
	var number [8]byte
	for _, line := range lines {
		writeDigestString(hash, line.Path, &number)
		writeDigestString(hash, line.Type, &number)
		binary.BigEndian.PutUint64(number[:], uint64(line.Size)) //nolint:gosec // Bit-preserving conversion for a digest, not arithmetic.
		_, _ = hash.Write(number[:])
		_, _ = hash.Write(line.SHA256[:])
	}
	var digest [32]byte
	copy(digest[:], hash.Sum(nil))
	return digest
}
func writeDigestString(hash interface {
	Write(data []byte) (int, error)
}, value string, number *[8]byte) {
	binary.BigEndian.PutUint64(number[:], uint64(len(value)))
	_, _ = hash.Write(number[:])
	_, _ = hash.Write([]byte(value))
}

func lineKey(line Line) string {
	return line.Path
}

func sameContent(left, right Line) bool {
	return left.Path == right.Path && left.Type == right.Type && left.Size == right.Size && left.SHA256 == right.SHA256
}

func sortLines(lines []Line) {
	sort.Slice(lines, func(i, j int) bool {
		if lines[i].Path != lines[j].Path {
			return lines[i].Path < lines[j].Path
		}
		return lines[i].Root < lines[j].Root
	})
}
