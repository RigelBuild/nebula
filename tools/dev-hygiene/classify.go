package main

import "time"

// verdict is what the cleanup does with one jj workspace.
type verdict string

const (
	// verdictDelete describes idle, clean, landed workspaces. The sweep reports them and does not delete them.
	verdictDelete verdict = "delete"
	// verdictReport: idle but dirty or holding unlanded commits. Never removed; build output only.
	verdictReport verdict = "report"
	// verdictRecent: touched within the idle window. Left alone.
	verdictRecent verdict = "recent"
	// verdictLive: a process has its working directory inside the workspace. Skipped.
	verdictLive verdict = "live"
	// verdictOrphaned: the .jj/repo pointer names a store that is gone. Kept in place; never forgotten or removed.
	verdictOrphaned verdict = "orphaned-store"
	// verdictForgotten: the store no longer lists this workspace, so jj cannot inspect it. Kept in place.
	verdictForgotten verdict = "forgotten"
	// verdictReclaimable includes clean, landed workspaces and old orphaned or forgotten ones; it is report-only.
	verdictReclaimable verdict = "reclaimable"
)

// workspace is the state classify decides on. Unlanded holds change IDs not in trunk.
type workspace struct {
	Name         string
	Root         string
	LastActive   time.Time
	Clean        bool
	Unlanded     []string
	LiveCwd      bool
	Notes        []string
	StoreMissing string
	// Forgotten is set when the store no longer lists this workspace. jj cannot judge it.
	Forgotten bool
}

// reclaimAge is how long an orphaned or forgotten workspace must stay idle before it is listed as reclaimable.
// Reclaimable is age-only plus forgotten or orphaned; no open-change check runs, so it stays a report, never a delete.
const reclaimAge = 7 * 24 * time.Hour

// classify applies the rules in order: live cwd, recency, a store jj cannot judge, then landed-and-clean.
// A workspace whose store is gone or no longer lists it is orphaned or forgotten while recent; once idle past
// reclaimAge it is reclaimable. Reclaimable is a report and never a delete.
func classify(ws workspace, now time.Time, idle time.Duration) verdict {
	if ws.LiveCwd {
		return verdictLive
	}
	if ws.LastActive.After(now.Add(-idle)) {
		return verdictRecent
	}
	if ws.StoreMissing != "" || ws.Forgotten {
		if ws.LastActive.Before(now.Add(-reclaimAge)) {
			return verdictReclaimable
		}
		if ws.StoreMissing != "" {
			return verdictOrphaned
		}
		return verdictForgotten
	}
	if !ws.Clean || len(ws.Unlanded) > 0 {
		return verdictReport
	}
	return verdictDelete
}
