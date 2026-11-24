// Package seats is the relauncher's seat step: it relaunches at most one idle non-root seat whose
// omp scope is over a memory threshold, through `cotal stop` and `wave-seats restart`.
package seats

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
)

const (
	gib                 = int64(1) << 30
	defaultThresholdGiB = 9

	// ThresholdEnv names the environment variable that overrides the threshold in whole GiB.
	ThresholdEnv = "WAVE_SEAT_RELAUNCH_THRESHOLD_GIB"

	outcomeRelaunched = "relaunched"
	outcomeWorking    = "skipped:working"
	outcomeSubagents  = "skipped:subagents"
)

// Candidate is one active seat the step may relaunch.
type Candidate struct {
	Name        string
	MemoryBytes int64
	Idle        bool
	Subagents   bool // true when live child sessions exist or state is unknown
}

// ThresholdBytes reads the threshold override. Empty uses the default; any other value must be a
// positive whole GiB count, or the step is skipped with a failure line.
func ThresholdBytes(override string) (int64, error) {
	if override == "" {
		return defaultThresholdGiB * gib, nil
	}
	value, err := strconv.ParseInt(override, 10, 64)
	if err != nil || value <= 0 || value > (1<<62)/gib {
		return 0, fmt.Errorf("%s must be a positive whole number of GiB, got %q", ThresholdEnv, override)
	}
	return value * gib, nil
}

// runNoops names the run outcomes that touched no seat. It is an allow-list: any other reason,
// including a new one, leaves the seats alone.
var runNoops = map[string]bool{
	"hold":               true,
	"root-seat-inactive": true,
	"starting":           true,
	"manager-stopping":   true,
	"restart-not-needed": true,
	"unchanged":          true,
}

// RunLeftSeatsFree reports whether the run outcome left the seats untouched, so the step may run.
func RunLeftSeatsFree(outcome, reason string) bool {
	return outcome == "skipped" && runNoops[reason]
}

// ordered drops the root seat and sorts the rest largest memory first.
func ordered(root string, seats []Candidate) []Candidate {
	kept := make([]Candidate, 0, len(seats))
	for _, seat := range seats {
		if seat.Name != root {
			kept = append(kept, seat)
		}
	}
	slices.SortStableFunc(kept, func(a, b Candidate) int { return cmp.Compare(b.MemoryBytes, a.MemoryBytes) })
	return kept
}

// CountsIdle is the idle signal after checking the current roster identity and explicit background idle state.
func CountsIdle(rosterIdle bool) bool {
	return rosterIdle
}
