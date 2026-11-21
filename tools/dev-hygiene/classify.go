package main

import "time"

// verdict is what the cleanup does with one jj workspace.
type verdict string

const (
	// verdictDelete: idle, clean, nothing unpushed. Forget and remove the directory.
	verdictDelete verdict = "delete"
	// verdictReport: idle but dirty or holding unpushed commits. Never removed; build output only.
	verdictReport verdict = "report"
	// verdictRecent: touched within the idle window. Left alone.
	verdictRecent verdict = "recent"
	// verdictLive: a process has its working directory inside the workspace. Skipped.
	verdictLive verdict = "live"
)

// workspace is the state classify decides on. Unpushed holds change IDs not yet on any remote bookmark.
// Stamp is the activity clock taken by the sweep's scan, before any jj call; a later move means a file edit since.
type workspace struct {
	Name       string
	Root       string
	LastActive time.Time
	Clean      bool
	Unpushed   []string
	LiveCwd    bool
	Notes      []string
	Stamp      time.Time
}

// classify applies the rules in order: live cwd, then recency, then pushed-and-clean.
func classify(ws workspace, now time.Time, idle time.Duration) verdict {
	if ws.LiveCwd {
		return verdictLive
	}
	if ws.LastActive.After(now.Add(-idle)) {
		return verdictRecent
	}
	if !ws.Clean || len(ws.Unpushed) > 0 {
		return verdictReport
	}
	return verdictDelete
}
