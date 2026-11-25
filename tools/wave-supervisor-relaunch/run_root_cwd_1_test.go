package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/RigelBuild/nebula/tools/go-lib/waveseats"
)

// The root seat's recorded cwd must select the session dir and the spawn cwd.
func TestRootSeatRecordedCWDDrivesSessionLookupAndSpawn(t *testing.T) {
	const rootCWD = "/home/example/agents/workspaces/foreman"
	seatsPath := filepath.Join(t.TempDir(), "wave-seats.json")
	list := waveseats.SeatListV2{Version: 2, Revision: 1, Seats: []waveseats.SeatEntryV2{{
		Name: "foreman", SeatRevision: 1, CWD: rootCWD, Tab: "foreman", State: waveseats.SeatV2Active,
		UpdatedAt: "2026-11-04T12:34:56.000Z", Profile: "worker", Role: "worker", Workspace: rootCWD, Persona: "foreman",
		Key: waveseats.KeySettings{Routing: map[waveseats.Provider]any{}, PolicyID: "policy"},
	}}}
	data, err := waveseats.SerializeSeatListV2(list, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(seatsPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	h := newRunHarness()
	h.fs.files = map[string][]byte{}
	h.deps.SeatNames = func(ctx context.Context) (map[string]struct{}, error) {
		return readActiveSeatNames(ctx, seatsPath, nil)
	}
	h.deps.ReadRootCWD = func(ctx context.Context) (string, error) {
		return readActiveRootCWD(ctx, seatsPath, nil)
	}
	// The root session lives in the dir slugged from the recorded cwd.
	rootSession := "/sessions/-agents-workspaces-foreman/root.jsonl"
	h.fs.files[rootSession] = []byte(`{"type":"session","id":"root-session","title":"foreman","cwd":"` + rootCWD + `"}`)
	h.fs.times[rootSession] = h.time
	// A newer decoy in the old fixed -agents dir must never be picked.
	decoy := "/sessions/-agents/decoy.jsonl"
	h.fs.files[decoy] = []byte(`{"type":"session","id":"decoy-session","title":"foreman","cwd":"/home/example/agents"}`)
	h.fs.times[decoy] = h.time.Add(1)
	var spawnCWD string
	spawn := h.deps.Spawn
	h.deps.Spawn = func(ctx context.Context, args []string, cwd string) (int, error) {
		spawnCWD = cwd
		return spawn(ctx, args, cwd)
	}
	got := runSupervisor(t.Context(), h.deps)
	if got.Outcome != "relaunched" || got.Session != "root-session" || spawnCWD != rootCWD {
		t.Fatalf("run=%#v spawnCWD=%q calls=%v", got, spawnCWD, h.calls)
	}
}
