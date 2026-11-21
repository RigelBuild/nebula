// SPDX-License-Identifier: MIT OR Apache-2.0

// Package cdrelease holds the CD release queue: the state a release pipeline
// reads and writes, and the store that keeps it in one ConfigMap.
package cdrelease

import (
	"fmt"
	"maps"
	"slices"
	"time"
)

// MaxSoloRetries is how many consecutive failed releases a target gets before it
// is parked. A parked target rides only a push or a kick (Q2 A).
const MaxSoloRetries = 2

// StallBound is how long a release may go without a heartbeat before the next
// enqueue takes it over (Q7 A). It must exceed the longest deploy step, because
// no later heartbeat is written.
const StallBound = 60 * time.Minute

// Release is the claim on the single release slot.
type Release struct {
	Pipeline   int64    `json:"pipeline,omitempty"`
	DispatchID string   `json:"dispatchId"`
	Commit     string   `json:"commit"`
	ClaimedAt  string   `json:"claimedAt"`
	Heartbeat  string   `json:"heartbeat,omitempty"`
	Targets    []string `json:"targets,omitempty"`
}

// State is the whole queue document stored in the ConfigMap.
type State struct {
	Pending            []string       `json:"pending"`
	Failed             map[string]int `json:"failed"`
	Release            *Release       `json:"release,omitempty"`
	LastReleasedCommit string         `json:"lastReleasedCommit,omitempty"`
	CheckedCommit      string         `json:"checkedCommit,omitempty"`
}

// Enqueue adds targets to Pending, sorted and deduped. It never touches Release.
func Enqueue(s State, targets []string) State {
	s.Pending = sortedUnion(s.Pending, targets)
	return s
}

// Kick moves every Failed entry into Pending and clears its count. It is the
// manual CD_KICK: a parked target rides the next release regardless of how many
// times it failed (Q2 A). It never touches Release.
func Kick(s State) State {
	if len(s.Failed) == 0 {
		return s
	}
	targets := make([]string, 0, len(s.Failed))
	for t := range s.Failed {
		targets = append(targets, t)
	}
	s.Pending = sortedUnion(s.Pending, targets)
	s.Failed = map[string]int{}
	return s
}

// Claim moves the batch Batch(s) into Release.Targets and clears Pending. It is
// a no-op when a release is already claimed. A push starts the release, so a
// parked target rides it (Q2 A).
func Claim(s State, r Release) State {
	if s.Release != nil {
		return s
	}
	batch := Batch(s)
	if len(batch) == 0 {
		return s
	}
	r.Targets = batch
	s.Release = &r
	s.Pending = []string{}
	return s
}

// Finish records one release's outcomes against Release.Targets, clears the
// in-flight batch, and returns the next batch (empty: go idle).
//
// A target in Targets that is neither succeeded nor failed counts as failed, so
// a batch a killed pipeline never reported is still retried, not lost. Each
// target counts once per release, however often a caller lists it in failed.
func Finish(s State, commit string, succeeded, failed []string) (State, []string) {
	var inFlight []string
	if s.Release != nil {
		inFlight = s.Release.Targets
	}
	counts := make(map[string]int, len(s.Failed))
	maps.Copy(counts, s.Failed)
	ok := make(map[string]bool, len(succeeded))
	for _, t := range succeeded {
		ok[t] = true
		delete(counts, t)
	}
	reported := make(map[string]bool, len(failed)+len(inFlight))
	for _, t := range failed {
		reported[t] = true
	}
	for _, t := range inFlight {
		if !ok[t] {
			reported[t] = true
		}
	}
	for t := range reported {
		counts[t]++
	}
	s.Failed = counts
	s.Release = nil
	s.LastReleasedCommit = commit
	next := Batch(s)
	return s, next
}

// Batch is Pending plus Failed entries with count < MaxSoloRetries, or every
// Failed entry when Pending is non-empty. A target that has failed MaxSoloRetries
// times in a row waits for a push or kick (Q2 A).
func Batch(s State) []string {
	if len(s.Pending) > 0 {
		return sortedUnion(s.Pending, allFailed(s))
	}
	return retryable(s)
}

func retryable(s State) []string {
	var out []string
	for t, n := range s.Failed {
		if n < MaxSoloRetries {
			out = append(out, t)
		}
	}
	slices.Sort(out)
	return out
}

func allFailed(s State) []string {
	out := make([]string, 0, len(s.Failed))
	for t := range s.Failed {
		out = append(out, t)
	}
	slices.Sort(out)
	return out
}

// Stalled reports whether the release is old enough to take over. The age is
// measured from the last heartbeat, or from claimedAt when no heartbeat is set.
func Stalled(r *Release, now time.Time) (bool, error) {
	if r == nil {
		return false, nil
	}
	ref := r.Heartbeat
	if ref == "" {
		ref = r.ClaimedAt
	}
	at, err := time.Parse(time.RFC3339, ref)
	if err != nil {
		return false, fmt.Errorf("parse release timestamp %q: %w", ref, err)
	}
	return now.Sub(at) > StallBound, nil
}

func sortedUnion(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, t := range list {
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	slices.Sort(out)
	if len(out) == 0 {
		return nil
	}
	return out
}
