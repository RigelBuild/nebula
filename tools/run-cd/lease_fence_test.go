package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/cdgate"
	"github.com/RigelBuild/nebula/tools/go-lib/cdrelease"
)

// fakeQueue is an in-memory release queue store. An error from fn is a
// refusal: nothing is written, as in the ConfigMap store.
type fakeQueue struct {
	state  cdrelease.State
	writes int
}

func (q *fakeQueue) Update(_ context.Context, fn func(cdrelease.State) (cdrelease.State, error)) (cdrelease.State, error) {
	next, err := fn(q.state)
	if err != nil {
		return cdrelease.State{}, err
	}
	q.writes++
	q.state = next
	return next, nil
}

// useQueue makes run-cd open q as the release queue store for this test.
func useQueue(t *testing.T, q *fakeQueue) {
	t.Helper()
	previous := queueStore
	queueStore = func(func(string) string) (cdrelease.Store, error) { return q, nil }
	t.Cleanup(func() { queueStore = previous })
}

func claimedQueue(pipeline int64, dispatchID string) *fakeQueue {
	return &fakeQueue{state: cdrelease.State{Release: &cdrelease.Release{
		Pipeline:   pipeline,
		DispatchID: dispatchID,
		ClaimedAt:  time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
	}}}
}

// fenceEnv is the environment of a step in pipeline 7 with dispatchID.
func fenceEnv(dispatchID string) func(string) string {
	env := map[string]string{"CI_PIPELINE_NUMBER": "7", cdgate.DispatchIDVariable: dispatchID}
	return func(name string) string { return env[name] }
}

// newRun wires the release fence for a --lease step, reading this pipeline's
// number and dispatch id from the environment.
func TestNewRunWiresLeaseFenceFromEnvironment(t *testing.T) {
	cases := map[string]struct {
		queue      *fakeQueue
		dispatchID string
		want       bool
		wantWrites int
	}{
		"current release":     {claimedQueue(7, "d7"), "d7", true, 1},
		"stale dispatch id":   {claimedQueue(7, "d7"), "old", false, 0},
		"no dispatch id":      {claimedQueue(7, "d7"), "", false, 0},
		"taken over pipeline": {claimedQueue(9, "d9"), "d7", false, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			useQueue(t, tc.queue)
			r := mustNewRun(t, options{target: "aws:up", lease: "cd-lease", members: []string{"m"}, command: []string{"true"}},
				fenceEnv(tc.dispatchID), &bytes.Buffer{})
			if r.Fence == nil {
				t.Fatal("newRun did not wire the release fence for a --lease step")
			}
			if r.PublishFence != nil {
				t.Fatal("newRun wired the publish fence on a --lease step")
			}
			got, err := r.Fence(t.Context())
			if err != nil {
				t.Fatalf("fence: %v", err)
			}
			if got != tc.want || tc.queue.writes != tc.wantWrites {
				t.Fatalf("fence = %v with %d writes, want %v with %d", got, tc.queue.writes, tc.want, tc.wantWrites)
			}
		})
	}
}

func TestNewRunWithoutLeaseWiresNoStoreFence(t *testing.T) {
	r := mustNewRun(t, options{target: "ci:publish", members: []string{"m"}, command: []string{"true"}},
		fenceEnv(""), &bytes.Buffer{})
	if r.Fence != nil || r.PublishFence != nil {
		t.Fatal("a step with no --lease and no dispatch id must take no fence")
	}
}

// --heartbeat writes one heartbeat for the current release and exits 0.
func TestHeartbeatModeWritesOnceForCurrentRelease(t *testing.T) {
	q := claimedQueue(7, "d7")
	useQueue(t, q)
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"--heartbeat"}, fenceEnv("d7"), &stderr); code != 0 {
		t.Fatalf("run --heartbeat = %d, want 0; stderr=%q", code, stderr.String())
	}
	if q.writes != 1 || q.state.Release.Heartbeat == "" {
		t.Fatalf("writes=%d heartbeat=%q, want one heartbeat", q.writes, q.state.Release.Heartbeat)
	}
}

// --heartbeat from a pipeline that is not the current release writes nothing
// and exits 0.
func TestHeartbeatModeFencedOutWritesNothing(t *testing.T) {
	q := claimedQueue(9, "d9")
	useQueue(t, q)
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"--heartbeat"}, fenceEnv("d7"), &stderr); code != 0 {
		t.Fatalf("run --heartbeat = %d, want 0; stderr=%q", code, stderr.String())
	}
	if q.writes != 0 {
		t.Fatalf("writes=%d, want none for a fenced-out pipeline", q.writes)
	}
}

func TestHeartbeatModeStoreErrorExitsOne(t *testing.T) {
	previous := queueStore
	queueStore = func(func(string) string) (cdrelease.Store, error) { return nil, errors.New("no cluster") }
	t.Cleanup(func() { queueStore = previous })
	if code := run(t.Context(), []string{"--heartbeat"}, fenceEnv("d7"), &bytes.Buffer{}); code != 1 {
		t.Fatalf("run --heartbeat = %d, want 1 when the store cannot be opened", code)
	}
}

// A cancelled step must not report success from --heartbeat, as the normal run
// path does. Here the pipeline is the current release, so the heartbeat path
// returns 0, and only the cancellation turns it into 1.
func TestHeartbeatModeCancelledContextExitsOne(t *testing.T) {
	q := claimedQueue(7, "d7")
	useQueue(t, q)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if code := run(ctx, []string{"--heartbeat"}, fenceEnv("d7"), &bytes.Buffer{}); code != 1 {
		t.Fatalf("run --heartbeat with a cancelled context = %d, want 1", code)
	}
}
