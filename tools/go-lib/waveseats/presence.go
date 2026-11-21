// SPDX-License-Identifier: MIT OR Apache-2.0

package waveseats

import (
	"encoding/json"
	"strings"

	"github.com/RigelBuild/nebula/tools/go-lib/cotalreach"
)

// ManagerStatusAvailable reports whether a manager status value names a state and a completed
// last sweep. Without both, seat liveness is unknown.
func ManagerStatusAvailable(status any) bool {
	data := managerStatusData(status)
	if data == nil {
		return false
	}
	if _, ok := data["state"].(string); !ok {
		return false
	}
	lastSweep, ok := data["lastSweep"].(map[string]any)
	if !ok {
		return false
	}
	completed, exists := lastSweep["completedAt"]
	return exists && completed != nil && completed != ""
}

// PresentNames returns the seats that are present: a cotal ps row, a running holder, or one of
// names with a retrying or retry-scheduled manager failure. `wave-seats restart` refuses a seat
// this returns, so every caller that waits for a seat to leave must use it.
func PresentNames(ps, holders string, status any, names []string) map[string]struct{} {
	present := make(map[string]struct{})
	_, rows, _ := cotalreach.ParsePsItems(ps)
	for _, row := range rows {
		var name string
		if json.Unmarshal(row["name"], &name) == nil {
			present[name] = struct{}{}
		}
	}
	for name := range runningHolderNames(holders) {
		present[name] = struct{}{}
	}
	failures, _ := managerStatusData(status)["failures"].([]any)
	for _, name := range names {
		if retryingFailure(failures, name) {
			present[name] = struct{}{}
		}
	}
	return present
}

// managerStatusData returns the static reconciliation block when present, else the envelope.
func managerStatusData(status any) map[string]any {
	envelope, _ := status.(map[string]any)
	if data, ok := envelope["staticReconciliation"].(map[string]any); ok {
		return data
	}
	return envelope
}

func retryingFailure(failures []any, name string) bool {
	for _, item := range failures {
		failure, ok := item.(map[string]any)
		if !ok || failure["alias"] != name {
			continue
		}
		phase, _ := failure["phase"].(string)
		disposition, _ := failure["disposition"].(string)
		if phase == "retrying" || phase == "retry-scheduled" || disposition == "retrying" || disposition == "retry-scheduled" {
			return true
		}
	}
	return false
}

// runningHolderNames parses `wave-seat-holder list --json` NDJSON and keeps running holders.
func runningHolderNames(ndjson string) map[string]struct{} {
	names := make(map[string]struct{})
	for line := range strings.SplitSeq(ndjson, "\n") {
		var row map[string]any
		if strings.TrimSpace(line) == "" || json.Unmarshal([]byte(line), &row) != nil || row == nil {
			continue
		}
		name, nameOK := row["name"].(string)
		status, statusOK := row["status"].(string)
		if nameOK && statusOK && status == "running" {
			names[name] = struct{}{}
		}
	}
	return names
}
