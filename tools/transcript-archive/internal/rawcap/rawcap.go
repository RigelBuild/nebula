// Package rawcap decides whether a transcript archive pass may run, from the repository's raw-data byte count.
package rawcap

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrOverCap reports that the repository holds at least the cap in raw bytes.
var ErrOverCap = errors.New("repository raw bytes at or above cap")

// statsOutput is the subset of `restic stats --mode raw-data --json` the cap reads.
type statsOutput struct {
	TotalSize *uint64 `json:"total_size"`
}

// Check returns nil when the raw-data size is below capBytes. It fails closed: an absent or unparseable size is an error, not a pass.
func Check(statsJSON []byte, capBytes uint64) error {
	var stats statsOutput
	if err := json.Unmarshal(statsJSON, &stats); err != nil {
		return fmt.Errorf("decode restic stats: %w", err)
	}
	if stats.TotalSize == nil {
		return errors.New("restic stats has no total_size")
	}
	if *stats.TotalSize >= capBytes {
		return fmt.Errorf("%w: %d >= %d", ErrOverCap, *stats.TotalSize, capBytes)
	}
	return nil
}
