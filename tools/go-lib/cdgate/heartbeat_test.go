package cdgate

import (
	"errors"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/cdrelease"
)

var heartbeatNow = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

// runningRelease returns a queue whose release is pipeline 7 with dispatch d7,
// claimed at claimedAt, and no heartbeat yet.
func runningRelease(claimedAt time.Time) cdrelease.State {
	return cdrelease.State{Release: &cdrelease.Release{
		Pipeline:   7,
		DispatchID: "d7",
		Commit:     "c7",
		ClaimedAt:  claimedAt.Format(time.RFC3339),
		Targets:    []string{"aws:up"},
	}}
}

func TestHeartbeatStampsTheCurrentRelease(t *testing.T) {
	s := runningRelease(heartbeatNow.Add(-time.Minute))
	got, err := Heartbeat(s, 7, "d7", heartbeatNow)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if want := heartbeatNow.Format(time.RFC3339); got.Release.Heartbeat != want {
		t.Fatalf("heartbeat = %q, want %q", got.Release.Heartbeat, want)
	}
	if len(got.Release.Targets) != 1 || got.Release.Targets[0] != "aws:up" || got.Release.ClaimedAt == "" {
		t.Fatalf("heartbeat dropped release fields: %+v", got.Release)
	}
	if s.Release.Heartbeat != "" {
		t.Fatal("Heartbeat changed its input; it must return a new state")
	}
}

func TestHeartbeatRefusesAPipelineThatIsNotTheCurrentRelease(t *testing.T) {
	cases := map[string]struct {
		state      cdrelease.State
		pipeline   int64
		dispatchID string
	}{
		"no release claimed":          {cdrelease.State{}, 7, "d7"},
		"stale dispatch id":           {runningRelease(heartbeatNow), 7, "old"},
		"taken over by a newer claim": {cdrelease.State{Release: &cdrelease.Release{Pipeline: 9, DispatchID: "d9"}}, 7, "d7"},
		"empty dispatch id":           {runningRelease(heartbeatNow), 7, ""},
		// A Woodpecker restart of the current release copies its variables, so
		// it carries the same dispatch id under a new pipeline number.
		"same dispatch id, other pipeline": {runningRelease(heartbeatNow), 8, "d7"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := Heartbeat(tc.state, tc.pipeline, tc.dispatchID, heartbeatNow)
			if !errors.Is(err, ErrNotCurrent) {
				t.Fatalf("Heartbeat err = %v, want ErrNotCurrent", err)
			}
			if got.Release != nil && got.Release.Heartbeat != "" {
				t.Fatalf("a refused heartbeat changed the release: %+v", got.Release)
			}
		})
	}
}

// An empty heartbeat falls back to claimedAt: a release claimed longer ago than
// the bound is takeover-eligible, and a recent claim is not.
func TestEmptyHeartbeatFallsBackToClaimedAt(t *testing.T) {
	recent := runningRelease(heartbeatNow.Add(-(cdrelease.StallBound - time.Minute)))
	if stalled, err := cdrelease.Stalled(recent.Release, heartbeatNow); err != nil || stalled {
		t.Fatalf("recent claim with no heartbeat: stalled=%v err=%v, want not stalled", stalled, err)
	}
	old := runningRelease(heartbeatNow.Add(-(cdrelease.StallBound + time.Minute)))
	if stalled, err := cdrelease.Stalled(old.Release, heartbeatNow); err != nil || !stalled {
		t.Fatalf("old claim with no heartbeat: stalled=%v err=%v, want stalled", stalled, err)
	}
}

// Once a heartbeat is written it governs: a recent heartbeat keeps an old claim
// alive, and an old heartbeat makes a recent claim takeover-eligible.
func TestHeartbeatOlderThanBoundMakesReleaseTakeoverEligible(t *testing.T) {
	alive := runningRelease(heartbeatNow.Add(-5 * time.Hour))
	alive.Release.Heartbeat = heartbeatNow.Add(-10 * time.Minute).Format(time.RFC3339)
	if stalled, err := cdrelease.Stalled(alive.Release, heartbeatNow); err != nil || stalled {
		t.Fatalf("fresh heartbeat on an old claim: stalled=%v err=%v, want not stalled", stalled, err)
	}
	silent := runningRelease(heartbeatNow.Add(-time.Minute))
	silent.Release.Heartbeat = heartbeatNow.Add(-(cdrelease.StallBound + time.Minute)).Format(time.RFC3339)
	if stalled, err := cdrelease.Stalled(silent.Release, heartbeatNow); err != nil || !stalled {
		t.Fatalf("old heartbeat on a recent claim: stalled=%v err=%v, want takeover-eligible", stalled, err)
	}
}
