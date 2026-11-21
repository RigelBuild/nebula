// SPDX-License-Identifier: MIT OR Apache-2.0

package issueowner

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/roster"
)

// RosterStatus describes whether an owner token is rostered or live.
type RosterStatus string

const (
	Rostered       RosterStatus = "rostered"
	LiveUnrostered RosterStatus = "live-unrostered"
	Unknown        RosterStatus = "unknown"
)

// RosterSet contains roster names, live workspace names, and load warnings.
type RosterSet struct {
	Rostered map[string]struct{}
	Live     map[string]struct{}
	Warnings []string
}

// LoadRosterSet loads rostered agents and live workspace names without failing the caller.
func LoadRosterSet(ctx context.Context, rosterPath, workspacesRoot string, now time.Time) RosterSet {
	result := RosterSet{
		Rostered: make(map[string]struct{}),
		Live:     make(map[string]struct{}),
		Warnings: []string{},
	}
	if rosterPath == "" {
		rosterPath = roster.Path(os.Getenv)
	}
	if workspacesRoot == "" {
		home := os.Getenv("HOME")
		if home == "" {
			home = "~"
		}
		workspacesRoot = home + "/agents/workspaces"
	}
	snapshot, err := roster.Load(ctx, rosterPath, now)
	if err != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf("canonical roster unavailable (%v)", err))
	} else {
		for _, name := range snapshot.Names() {
			result.Rostered[name] = struct{}{}
		}
	}

	entries, err := os.ReadDir(workspacesRoot)
	if err != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf("workspace root %s could not be listed (%v)", workspacesRoot, err))
		return result
	}
	for _, entry := range entries {
		if entry.IsDir() {
			result.Live[entry.Name()] = struct{}{}
		}
	}
	return result
}

// Status classifies a codename, with roster membership taking precedence.
func (r RosterSet) Status(token string) RosterStatus {
	if _, ok := r.Rostered[token]; ok {
		return Rostered
	}
	if _, ok := r.Live[token]; ok {
		return LiveUnrostered
	}
	return Unknown
}
