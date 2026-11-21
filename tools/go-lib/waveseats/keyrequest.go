// SPDX-License-Identifier: MIT OR Apache-2.0

package waveseats

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// ParseKeyRequest parses a request with only routing and limits.
func ParseKeyRequest(text string) (KeyRequest, error) {
	var raw json.RawMessage
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return KeyRequest{}, fmt.Errorf("key: invalid JSON: %w", err)
	}
	row, err := v2Object(raw, "key", "routing", "limits")
	if err != nil {
		return KeyRequest{}, err
	}
	wrapped, err := json.Marshal(map[string]any{"routing": row["routing"], "limits": row["limits"], "policyId": "import"})
	if err != nil {
		return KeyRequest{}, err
	}
	key, err := validateV2Key(wrapped, "key")
	if err != nil {
		return KeyRequest{}, err
	}
	return KeyRequest{Routing: key.Routing, Limits: key.Limits}, nil
}

func containsString(values []string, target string) bool {
	return slices.Contains(values, target)
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// NormalizeStateForCompatibility maps the legacy dormant state to v2 paused.
func NormalizeStateForCompatibility(state string) SeatStateV2 {
	if strings.EqualFold(state, "dormant") {
		return SeatV2Paused
	}
	return SeatStateV2(state)
}
