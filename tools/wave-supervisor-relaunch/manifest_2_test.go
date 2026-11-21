package main

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/relaunch"
)

func testProcessCensusAndAtomicManifestUsesParentFallbackAndMarksA(t *testing.T) {
	t.Helper()
	fsys := newMemoryFS()
	fsys.files["/proc-fixture/124/cgroup"] = []byte("0::/user.slice/wave-agents.slice/scope.scope")
	fsys.files["/proc-fixture/124/cmdline"] = []byte("omp\x00--fork\x00parent-id\x00")
	fsys.files["/proc-fixture/124/environ"] = []byte("COTAL_AGENT_FILE=/home/test/.cotal/agents/accounts.md\x00HOME=/home/test\x00")
	fsys.links["/proc-fixture/124/cwd"] = "/home/test/project"
	fsys.files["/sessions/-project/older.jsonl"] = []byte("{\"type\":\"session\",\"id\":\"older\",\"parentSession\":\"parent-id\"}")
	fsys.files["/sessions/-project/newer.jsonl"] = []byte("{\"type\":\"session\",\"id\":\"newer\",\"parentSession\":\"parent-id\"}")
	fsys.times["/sessions/-project/older.jsonl"] = time.UnixMilli(1)
	fsys.times["/sessions/-project/newer.jsonl"] = time.UnixMilli(2)

	census, err := readWaveManifest("/proc-fixture", fsys, "/sessions")
	if err != nil {
		t.Fatal(err)
	}
	want := []relaunch.WaveAgent{{Name: "accounts", SessionID: "newer"}}
	if !slices.Equal(census.Agents, want) {
		t.Fatalf("readWaveManifest().Agents = %#v, want %#v", census.Agents, want)
	}
	delete(fsys.files, "/sessions/-project/older.jsonl")
	delete(fsys.files, "/sessions/-project/newer.jsonl")
	census, err = readWaveManifest("/proc-fixture", fsys, "/sessions")
	if err != nil {
		t.Fatal(err)
	}
	want = []relaunch.WaveAgent{{Name: "accounts", SessionID: "parent-id", Stale: true}}
	if !slices.Equal(census.Agents, want) {
		t.Fatalf("readWaveManifest().Agents = %#v, want %#v", census.Agents, want)
	}
}

func testProcessCensusAndAtomicManifestWritesManifestsAndStateThroughTemporary(t *testing.T) {
	t.Helper()
	fsys := newMemoryFS()
	err := writeWaveManifest("/state/manifest.json", WaveManifest{
		At:          "now",
		Fingerprint: "boot:m:z",
		Agents:      []relaunch.WaveAgent{{Name: "foreman", SessionID: "child"}},
	}, fsys)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fsys.files["/state/manifest.json"]; !ok {
		t.Fatal("manifest was not persisted")
	}
	for path := range fsys.files {
		if strings.Contains(path, ".tmp") {
			t.Fatalf("temporary manifest remains at %q", path)
		}
	}
	state := relaunch.EmptyState()
	if err := relaunch.WriteState("/state/state.json", state, fsys); err != nil {
		t.Fatal(err)
	}
	got, valid := relaunch.ReadState("/state/state.json", fsys)
	if !valid || !reflect.DeepEqual(got, state) {
		t.Fatalf("ReadState() = %#v, %v, want %#v, true", got, valid, state)
	}
	fsys.files["/state/state.json"] = []byte("{")
	if _, valid := relaunch.ReadState("/state/state.json", fsys); valid {
		t.Fatal("ReadState() accepted malformed state")
	}
}

func testProcessCensusAndAtomicManifestReadsExpiresAndRemovesHolds(t *testing.T) {
	t.Helper()
	fsys := newMemoryFS()
	fsys.files["/hold"] = []byte(`{"until":"2026-11-01T00:01:00Z","reason":"operator"}`)
	now := time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)
	hold, expired, err := relaunch.ReadHold("/hold", now, fsys)
	if err != nil || expired || hold == nil || *hold != (relaunch.Hold{Until: "2026-11-01T00:01:00.000Z", Reason: "operator"}) {
		t.Fatalf("ReadHold() = %#v, %v, %v", hold, expired, err)
	}
	hold, expired, err = relaunch.ReadHold("/hold", now.Add(2*time.Minute), fsys)
	if err != nil || !expired || hold != nil {
		t.Fatalf("expired ReadHold() = %#v, %v, %v, want nil, true, nil", hold, expired, err)
	}
}
