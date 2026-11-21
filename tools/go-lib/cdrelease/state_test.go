package cdrelease

import (
	"reflect"
	"testing"
	"time"
)

func TestEnqueueAddsSortedDedupedPendingAndNeverTouchesRelease(t *testing.T) {
	in := State{
		Pending: []string{"b"},
		Failed:  map[string]int{"x": 1},
		Release: &Release{Commit: "c1", Targets: []string{"r"}},
	}
	got := Enqueue(in, []string{"aws:up", "b", "aws:up"})
	if want := []string{"aws:up", "b"}; !reflect.DeepEqual(got.Pending, want) {
		t.Fatalf("Pending = %v, want %v", got.Pending, want)
	}
	if !reflect.DeepEqual(got.Failed, map[string]int{"x": 1}) {
		t.Fatalf("Failed = %v, want unchanged", got.Failed)
	}
	if got.Release == nil || !reflect.DeepEqual(got.Release.Targets, []string{"r"}) {
		t.Fatalf("Release = %+v, want unchanged while a release runs", got.Release)
	}
}

// A kick moves every Failed entry into Pending and clears its count, so a
// parked target rides the next release (Q2 A). Pending keeps its own targets,
// and Release is untouched.
func TestKickMovesEveryFailedEntryToPending(t *testing.T) {
	in := State{
		Pending: []string{"b"},
		Failed:  map[string]int{"x": 2, "y": 1},
		Release: &Release{Commit: "c1", Targets: []string{"r"}},
	}
	got := Kick(in)
	if want := []string{"b", "x", "y"}; !reflect.DeepEqual(got.Pending, want) {
		t.Fatalf("Pending = %v, want %v", got.Pending, want)
	}
	if len(got.Failed) != 0 {
		t.Fatalf("Failed = %v, want every entry moved out", got.Failed)
	}
	if got.Release == nil || !reflect.DeepEqual(got.Release.Targets, []string{"r"}) {
		t.Fatalf("Release = %+v, want unchanged", got.Release)
	}
}

// A kick with nothing failed changes nothing.
func TestKickWithNoFailedEntriesIsNoOp(t *testing.T) {
	in := State{Pending: []string{"b"}}
	got := Kick(in)
	if want := []string{"b"}; !reflect.DeepEqual(got.Pending, want) {
		t.Fatalf("Pending = %v, want %v", got.Pending, want)
	}
}

func TestBatchRetriesFailedOnceAndParksSecondFailure(t *testing.T) {
	cases := []struct {
		name string
		in   State
		want []string
	}{
		{
			name: "pending only",
			in:   State{Pending: []string{"a"}},
			want: []string{"a"},
		},
		{
			name: "failed once is retried without pending",
			in:   State{Failed: map[string]int{"a": 1}},
			want: []string{"a"},
		},
		{
			name: "failed twice stays parked without pending",
			in:   State{Failed: map[string]int{"a": 2}},
			want: nil,
		},
		{
			name: "failed twice rides a push that has pending",
			in:   State{Pending: []string{"b"}, Failed: map[string]int{"a": 2}},
			want: []string{"a", "b"},
		},
		{
			name: "pending and retryable failed union, sorted, deduped",
			in:   State{Pending: []string{"b"}, Failed: map[string]int{"a": 1, "b": 1}},
			want: []string{"a", "b"},
		},
		{
			name: "empty state is empty batch",
			in:   State{},
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Batch(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Batch = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestClaimMovesBatchIntoTargetsAndClearsPending(t *testing.T) {
	in := State{Pending: []string{"b", "a"}, Failed: map[string]int{"c": 1, "p": 2}}
	got := Claim(in, Release{DispatchID: "d1", Commit: "head", ClaimedAt: "2026-11-18T10:00:00Z"})
	if got.Release == nil {
		t.Fatal("Claim left Release nil")
	}
	// A push starts the release, so a parked target (p, two failures) rides it (Q2 A).
	if want := []string{"a", "b", "c", "p"}; !reflect.DeepEqual(got.Release.Targets, want) {
		t.Fatalf("Release.Targets = %v, want %v", got.Release.Targets, want)
	}
	if got.Release.DispatchID != "d1" || got.Release.Commit != "head" || got.Release.ClaimedAt != "2026-11-18T10:00:00Z" {
		t.Fatalf("Release = %+v, want the claim fields as given", got.Release)
	}
	if len(got.Pending) != 0 {
		t.Fatalf("Pending = %v, want cleared", got.Pending)
	}
	if got.Failed["c"] != 1 {
		t.Fatalf("Failed = %v, want the in-flight entry kept until Finish", got.Failed)
	}
}

func TestClaimIsNoOpWhenReleaseIsSet(t *testing.T) {
	in := State{
		Pending: []string{"a"},
		Release: &Release{DispatchID: "running", Targets: []string{"z"}},
	}
	got := Claim(in, Release{DispatchID: "other"})
	if got.Release == nil || got.Release.DispatchID != "running" {
		t.Fatalf("Release = %+v, want the running claim kept", got.Release)
	}
	if !reflect.DeepEqual(got.Pending, []string{"a"}) {
		t.Fatalf("Pending = %v, want untouched", got.Pending)
	}
}

func TestClaimWithEmptyBatchTakesNoSlot(t *testing.T) {
	in := State{Failed: map[string]int{"a": 2}}
	got := Claim(in, Release{DispatchID: "d"})
	if got.Release != nil {
		t.Fatalf("Release = %+v, want nil: a parked-only state has nothing to run", got.Release)
	}
}

func TestFinishRecordsOutcomesClearsReleaseKeepsPending(t *testing.T) {
	in := State{
		Pending: []string{"new"},
		Failed:  map[string]int{"ok": 1, "bad": 1},
		Release: &Release{DispatchID: "d", Commit: "c", Targets: []string{"ok", "bad", "fresh"}},
	}
	got, next := Finish(in, "c", []string{"ok", "fresh"}, []string{"bad"})
	if got.Release != nil {
		t.Fatalf("Release = %+v, want nil after Finish", got.Release)
	}
	if got.LastReleasedCommit != "c" {
		t.Fatalf("LastReleasedCommit = %q, want c", got.LastReleasedCommit)
	}
	if want := map[string]int{"bad": 2}; !reflect.DeepEqual(got.Failed, want) {
		t.Fatalf("Failed = %v, want %v (success clears, failure counts)", got.Failed, want)
	}
	if !reflect.DeepEqual(got.Pending, []string{"new"}) {
		t.Fatalf("Pending = %v, want the push that arrived during the release kept", got.Pending)
	}
	if want := []string{"bad", "new"}; !reflect.DeepEqual(next, want) {
		t.Fatalf("next batch = %v, want %v (Pending is non-empty, so every Failed entry rides; Q2 A)", next, want)
	}
}

func TestFinishCountsUnreportedBatchTargetAsFailed(t *testing.T) {
	in := State{Release: &Release{Targets: []string{"a", "b"}}}
	got, next := Finish(in, "c", []string{"a"}, nil)
	if got.Failed["b"] != 1 {
		t.Fatalf("Failed = %v, want b counted failed when it was not reported", got.Failed)
	}
	if want := []string{"b"}; !reflect.DeepEqual(next, want) {
		t.Fatalf("next batch = %v, want %v (count 1 is retried)", next, want)
	}
}

func TestFinishReturnsEmptyBatchToGoIdle(t *testing.T) {
	in := State{Release: &Release{Targets: []string{"a"}}}
	got, next := Finish(in, "c", []string{"a"}, nil)
	if len(next) != 0 {
		t.Fatalf("next batch = %v, want empty so the store goes idle", next)
	}
	if got.Release != nil {
		t.Fatalf("Release = %+v, want nil", got.Release)
	}
}

// A caller that reports the same failed target twice must not park it after one release.
func TestFinishDedupesDuplicateFailedReport(t *testing.T) {
	in := State{Release: &Release{Targets: []string{"a"}}}
	got, next := Finish(in, "c", nil, []string{"a", "a"})
	if got.Failed["a"] != 1 {
		t.Fatalf("Failed = %v, want a counted once for one release", got.Failed)
	}
	if want := []string{"a"}; !reflect.DeepEqual(next, want) {
		t.Fatalf("next batch = %v, want %v (count 1 is retried)", next, want)
	}
}

func TestStalledUsesHeartbeatThenClaimedAt(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		rel  *Release
		want bool
	}{
		{name: "no release is not stalled", rel: nil, want: false},
		{
			name: "fresh heartbeat is not stalled",
			rel:  &Release{ClaimedAt: "2026-11-18T10:00:00Z", Heartbeat: "2026-11-18T11:30:00Z"},
			want: false,
		},
		{
			name: "heartbeat older than the bound is stalled even if claim is fresh",
			rel:  &Release{ClaimedAt: "2026-11-18T11:59:00Z", Heartbeat: "2026-11-18T10:30:00Z"},
			want: true,
		},
		{
			name: "empty heartbeat falls back to claimedAt, old claim is stalled",
			rel:  &Release{ClaimedAt: "2026-11-18T10:30:00Z"},
			want: true,
		},
		{
			name: "empty heartbeat falls back to claimedAt, recent claim is not",
			rel:  &Release{ClaimedAt: "2026-11-18T11:45:00Z"},
			want: false,
		},
		{
			name: "age exactly the bound is not stalled",
			rel:  &Release{ClaimedAt: "2026-11-18T11:00:00Z"},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Stalled(tc.rel, now)
			if err != nil {
				t.Fatalf("Stalled: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Stalled = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStalledRejectsMalformedTimestamp(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	if _, err := Stalled(&Release{ClaimedAt: "yesterday"}, now); err == nil {
		t.Fatal("Stalled accepted a non-RFC3339 claimedAt; want an error")
	}
}
