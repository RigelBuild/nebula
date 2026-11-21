// SPDX-License-Identifier: MIT OR Apache-2.0

// Package waveseats manages the journaled v2 desired seat list.
package waveseats

import (
	"os"
	"path/filepath"
	"regexp"
)

// ForemanRoot is the one root seat: the active foreman receives notices and is never reconciled.
const ForemanRoot = "foreman"

const sharedRouting = "shared"

const maxSafeInteger = 1<<53 - 1

var v2SeatNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

var v2TimestampPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z$`)

// SeatsPath returns WAVE_SEATS_PATH or the default desired-seat path.
func SeatsPath() string {
	if path, ok := os.LookupEnv("WAVE_SEATS_PATH"); ok {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, "agents", "state", "wave-seats.json")
}

// Provider identifies a supported model provider.
type Provider string

const (
	ProviderClaude Provider = "claude"
	ProviderCodex  Provider = "codex"
)

// Window limits a key's spend over a rolling or fixed interval.
type Window struct {
	Kind      string `json:"kind"`
	Seconds   uint64 `json:"seconds"`
	AnchorUTC string `json:"anchorUtc,omitempty"`
	Timezone  string `json:"timezone,omitempty"`
}

// ModelLimit sets optional per-model key limits.
type ModelLimit struct {
	Tokens *uint64 `json:"tokens,omitempty"`
	TPM    *uint64 `json:"tpm,omitempty"`
	RPM    *uint64 `json:"rpm,omitempty"`
}

// KeyCap limits spend over a window.
type KeyCap struct {
	USD    float64 `json:"usd"`
	Window Window  `json:"window"`
}

// KeyLimits contains spend and model restrictions.
type KeyLimits struct {
	KeyCap     *KeyCap               `json:"keyCap,omitempty"`
	PerModel   map[string]ModelLimit `json:"perModel,omitempty"`
	modelOrder []string
}

// KeySettings contains routing, limits and the selected policy.
type KeySettings struct {
	Routing  map[Provider]any `json:"routing"`
	Limits   KeyLimits        `json:"limits"`
	PolicyID string           `json:"policyId"`
}

// SeatStateV2 is the v2 desired lifecycle state.
type SeatStateV2 string

const (
	SeatStateUnknown SeatStateV2 = ""
	SeatV2Active     SeatStateV2 = "active"
	SeatV2Paused     SeatStateV2 = "paused"
	SeatV2Retired    SeatStateV2 = "retired"
)

// SeatHost identifies the machine assigned to a seat.
type SeatHost string

// HostSet is the set of host names a seat's host field may take. The caller builds it from its
// own configuration and passes it to every function that parses or loads a seat list. An empty
// or nil set refuses every host, so a caller that configures no hosts cannot accept a seat that
// names one.
type HostSet map[SeatHost]struct{}

// SeatEntryV2 describes a single v2 seat.
type SeatEntryV2 struct {
	Name              string      `json:"name"`
	SeatRevision      uint64      `json:"seatRevision"`
	CWD               string      `json:"cwd"`
	Tab               string      `json:"tab"`
	State             SeatStateV2 `json:"state"`
	RestartGeneration uint64      `json:"restartGeneration"`
	UpdatedAt         string      `json:"updatedAt"`
	Profile           string      `json:"profile"`
	Role              string      `json:"role"`
	RoleTreeRevision  uint64      `json:"roleTreeRevision"`
	Key               KeySettings `json:"key"`
	Workspace         string      `json:"workspace"`
	Persona           string      `json:"persona"`
	Parent            *string     `json:"parent,omitempty"`
	Note              *string     `json:"note,omitempty"`
	Host              *SeatHost   `json:"host,omitempty"`
}

// SeatMutation is the result recorded for an idempotent operation.
type SeatMutation struct {
	Revision          uint64      `json:"revision"`
	Entry             SeatEntryV2 `json:"entry"`
	PolicyDigest      *string     `json:"policyDigest,omitempty"`
	EntitlementDigest *string     `json:"entitlementDigest,omitempty"`
}

// OperationResult stores the first result for a principal and operation ID.
type OperationResult struct {
	RequestDigest string       `json:"requestDigest"`
	Result        SeatMutation `json:"result"`
}

// SeatListV2 is the version 2 desired-seat document.
type SeatListV2 struct {
	Version        int                        `json:"version"`
	Revision       uint64                     `json:"revision"`
	GenerationBase uint64                     `json:"generationBase,omitempty"`
	Seats          []SeatEntryV2              `json:"seats"`
	Operations     map[string]OperationResult `json:"operations,omitempty"`
	operationOrder []string
}

// V1Entry is a legacy seat that can be converted during the one-time import.
type V1Entry struct {
	Name      string
	CWD       string
	State     string
	UpdatedAt string
	Parent    *string
	Note      *string
}

// KeyRequest contains requested routing and limits before a policy is selected.
type KeyRequest struct {
	Routing map[Provider]any `json:"routing"`
	Limits  KeyLimits        `json:"limits"`
}

// ImportSeatFields contains every v2-only value required to import a legacy seat.
type ImportSeatFields struct {
	Tab              string
	Profile          string
	Role             string
	RoleTreeRevision uint64
	Workspace        string
	Persona          string
	Key              KeyRequest
}

// RolePolicy maps a role to its team and key policy.
type RolePolicy struct {
	Team     string `json:"team"`
	PolicyID string `json:"policyId"`
}

// ImportInputs supplies v2-only seat data and policy mappings for v1 import.
type ImportInputs struct {
	Policy map[string]RolePolicy
	Seats  map[string]ImportSeatFields
}

// SeatEntryV2ByName returns the desired v2 entry for a seat name.
func SeatEntryV2ByName(list SeatListV2, name string) (SeatEntryV2, bool) {
	for _, seat := range list.Seats {
		if seat.Name == name {
			return seat, true
		}
	}
	return SeatEntryV2{}, false
}
