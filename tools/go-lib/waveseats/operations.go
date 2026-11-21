// SPDX-License-Identifier: MIT OR Apache-2.0

package waveseats

import (
	"fmt"
	"slices"
	"strings"
)

// ActiveRoot selects the root: foreman when it is active, else empty. Any other seat is not a root.
func ActiveRoot(seats []SeatEntryV2) string {
	for _, seat := range seats {
		if seat.State == SeatV2Active && seat.Name == ForemanRoot {
			return ForemanRoot
		}
	}
	return ""
}

// ActiveRootNames selects the root from a set of active seat names.
func ActiveRootNames(names map[string]struct{}) string {
	if _, foremanActive := names[ForemanRoot]; foremanActive {
		return ForemanRoot
	}
	return ""
}

// OperationRequest builds the stable request payload used by idempotent CLI mutations.
func OperationRequest(command, name string, options map[string]string) string {
	keys := make([]string, 0, len(options))
	for key := range options {
		if key != "--operation-id" {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	fields := make([]string, 0, len(keys))
	for _, key := range keys {
		fields = append(fields, fmt.Sprintf("[%q,%q]", key, options[key]))
	}
	return fmt.Sprintf("[%q,%q,[%s]]", command, name, strings.Join(fields, ","))
}
