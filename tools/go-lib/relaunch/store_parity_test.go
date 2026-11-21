package relaunch

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"testing"
	"time"
)

func TestStoreParityReadState(t *testing.T) {
	t.Run("missing state returns a fresh empty value", func(t *testing.T) {
		got, valid := ReadState("/missing", newTestFS())
		if !valid || got.Version != 1 || len(got.Spawns) != 0 || len(got.Burned) != 0 {
			t.Fatalf("ReadState() = %#v, %t", got, valid)
		}
	})

	tests := []struct {
		name        string
		json        string
		valid       bool
		spawns      []string
		fingerprint string
		notice      string
		burned      []string
	}{
		{name: "valid empty arrays", json: `{"version":1,"spawns":[],"burned":[]}`, valid: true},
		{name: "strings preserved", json: `{"version":1,"spawns":["2026-01-02T03:04:05.000Z"],"burned":["session"]}`, valid: true, spawns: []string{"2026-01-02T03:04:05.000Z"}, burned: []string{"session"}},
		{name: "optional strings preserved", json: `{"version":1,"spawns":[],"burned":[],"lastFingerprint":"boot:manager","lastNoticeAt":"2026-01-02T03:04:05Z"}`, valid: true, fingerprint: "boot:manager", notice: "2026-01-02T03:04:05Z"},
		{name: "wrong version rejected", json: `{"version":2,"spawns":[],"burned":[]}`},
		{name: "missing spawns rejected", json: `{"version":1,"burned":[]}`},
		{name: "non-array spawns rejected", json: `{"version":1,"spawns":null,"burned":[]}`},
		{name: "non-string non-number spawn rejected", json: `{"version":1,"spawns":[null],"burned":[]}`},
		{name: "missing burned rejected", json: `{"version":1,"spawns":[]}`},
		{name: "non-string burned rejected", json: `{"version":1,"spawns":[],"burned":[1]}`},
		{name: "null fingerprint rejected", json: `{"version":1,"spawns":[],"burned":[],"lastFingerprint":null}`},
		{name: "numeric fingerprint rejected", json: `{"version":1,"spawns":[],"burned":[],"lastFingerprint":1}`},
		{name: "null notice rejected", json: `{"version":1,"spawns":[],"burned":[],"lastNoticeAt":null}`},
		{name: "non-object rejected", json: `[]`},
		{name: "invalid JSON rejected", json: `{`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := newTestFS()
			fsys.files["/state"] = []byte(tt.json)
			got, valid := ReadState("/state", fsys)
			if valid != tt.valid {
				t.Fatalf("ReadState() valid = %t, want %t", valid, tt.valid)
			}
			if !valid {
				return
			}
			if fmt.Sprint(got.Spawns) != fmt.Sprint(tt.spawns) || fmt.Sprint(got.Burned) != fmt.Sprint(tt.burned) || got.LastFingerprint != tt.fingerprint || got.LastNoticeAt != tt.notice {
				t.Fatalf("ReadState() = %#v", got)
			}
		})
	}
}

func TestStoreParityNumericSpawnDates(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
		valid bool
	}{
		{name: "epoch has millisecond precision", input: `0`, want: "1970-01-01T00:00:00.000Z", valid: true},
		{name: "positive fractional milliseconds truncate", input: `1.9`, want: "1970-01-01T00:00:00.001Z", valid: true},
		{name: "negative fractional milliseconds truncate toward zero", input: `-1.9`, want: "1969-12-31T23:59:59.999Z", valid: true},
		{name: "maximum TimeClip value", input: `8640000000000000`, want: "+275760-09-13T00:00:00.000Z", valid: true},
		{name: "minimum TimeClip value", input: `-8640000000000000`, want: "-271821-04-20T00:00:00.000Z", valid: true},
		{name: "above TimeClip rejected", input: `8640000000000001`},
		{name: "below TimeClip rejected", input: `-8640000000000001`},
		{name: "NaN rejected", input: `NaN`},
		{name: "positive infinity rejected", input: `1e999`},
		{name: "negative infinity rejected", input: `-1e999`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := newTestFS()
			fsys.files["/state"] = []byte(`{"version":1,"spawns":[` + tt.input + `],"burned":[]}`)
			got, valid := ReadState("/state", fsys)
			if valid != tt.valid {
				t.Fatalf("ReadState() valid = %t, want %t", valid, tt.valid)
			}
			if valid && (len(got.Spawns) != 1 || got.Spawns[0] != tt.want) {
				t.Fatalf("ReadState() spawns = %q, want %q", got.Spawns, tt.want)
			}
		})
	}
}

func TestStoreParityRestartRequest(t *testing.T) {
	now, err := time.Parse(time.RFC3339, "2026-11-03T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		body     string
		stale    bool
		cause    string
		identity string
	}{
		{name: "missing cause defaults to wedge", body: `{"at":"2026-11-03T00:00:00Z","reason":"test"}`, cause: "wedge"},
		{name: "null cause is stale", body: `{"at":"2026-11-03T00:00:00Z","reason":"test","cause":null}`, stale: true},
		{name: "number cause is stale", body: `{"at":"2026-11-03T00:00:00Z","reason":"test","cause":1}`, stale: true},
		{name: "boolean cause is stale", body: `{"at":"2026-11-03T00:00:00Z","reason":"test","cause":false}`, stale: true},
		{name: "unknown cause is stale", body: `{"at":"2026-11-03T00:00:00Z","reason":"test","cause":"other"}`, stale: true},
		{name: "wedge cause accepted", body: `{"at":"2026-11-03T00:00:00Z","reason":"test","cause":"wedge"}`, cause: "wedge"},
		{name: "RSS keeps identity string unchanged", body: `{"at":"2026-11-03T00:00:00Z","reason":"rss","cause":"rss","identity":"000123:000456"}`, cause: "rss", identity: "000123:000456"},
		{name: "RSS missing identity is stale", body: `{"at":"2026-11-03T00:00:00Z","reason":"rss","cause":"rss"}`, stale: true},
		{name: "RSS invalid identity is stale", body: `{"at":"2026-11-03T00:00:00Z","reason":"rss","cause":"rss","identity":"123:x"}`, stale: true},
		{name: "blank reason is stale", body: `{"at":"2026-11-03T00:00:00Z","reason":"  "}`, stale: true},
		{name: "future request is stale", body: `{"at":"2026-11-03T00:00:01Z","reason":"test"}`, stale: true},
		{name: "expired request is stale", body: `{"at":"2026-11-02T23:30:00Z","reason":"test"}`, stale: true},
		{name: "malformed json is stale", body: `{`, stale: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := newTestFS()
			fsys.files["/restart"] = []byte(tt.body)
			got, stale, err := ReadRestartRequest("/restart", now, fsys)
			if err != nil || stale != tt.stale {
				t.Fatalf("ReadRestartRequest() = %#v, %t, %v", got, stale, err)
			}
			if tt.stale {
				if got != nil {
					t.Fatalf("stale request = %#v", got)
				}
				return
			}
			if got == nil || got.Cause != tt.cause || got.Identity != tt.identity {
				t.Fatalf("request = %#v, want cause=%q identity=%q", got, tt.cause, tt.identity)
			}
			if got.At != "2026-11-03T00:00:00.000Z" {
				t.Fatalf("normalized time = %q", got.At)
			}
		})
	}
}

func TestStoreParityHoldAndRemoval(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-11-03T00:00:00Z")
	tests := []struct {
		name, body            string
		wantHold, wantExpired bool
	}{
		{name: "valid active hold", body: `{"until":"2026-11-03T00:01:00Z","reason":"operator"}`, wantHold: true},
		{name: "expired hold", body: `{"until":"2026-11-02T23:59:00Z","reason":"operator"}`, wantExpired: true},
		{name: "malformed hold ignored", body: `{`},
		{name: "invalid timestamp is not expired", body: `{"until":"not-a-date","reason":"operator"}`},
		{name: "missing fields ignored", body: `{"until":"2026-11-03T00:01:00Z"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := newTestFS()
			fsys.files["/hold"] = []byte(tt.body)
			got, expired, err := ReadHold("/hold", now, fsys)
			if err != nil || expired != tt.wantExpired || (got != nil) != tt.wantHold {
				t.Fatalf("ReadHold() = %#v,%t,%v", got, expired, err)
			}
			if got != nil && got.Until != "2026-11-03T00:01:00.000Z" {
				t.Fatalf("hold until = %q", got.Until)
			}
		})
	}
	fsys := newTestFS()
	if err := ReleaseHold("/missing", fsys); err != nil {
		t.Fatalf("ReleaseHold(missing) = %v", err)
	}
	if err := ReleaseRestartRequest("/missing", fsys); err != nil {
		t.Fatalf("ReleaseRestartRequest(missing) = %v", err)
	}
	fsys.files["/hold"] = []byte("hold")
	fsys.files["/restart"] = []byte("request")
	if err := ReleaseHold("/hold", fsys); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseRestartRequest("/restart", fsys); err != nil {
		t.Fatal(err)
	}
	if _, ok := fsys.files["/hold"]; ok {
		t.Fatal("hold remains after release")
	}
	if _, ok := fsys.files["/restart"]; ok {
		t.Fatal("request remains after release")
	}
}

func TestStoreParityWriteRoundTripsAtomically(t *testing.T) {
	fsys := newTestFS()
	state := RelaunchState{Version: 1, Spawns: []string{}, Burned: []string{}, LastFingerprint: "boot:manager"}
	if err := WriteState("/state/state.json", state, fsys); err != nil {
		t.Fatal(err)
	}
	got, valid := ReadState("/state/state.json", fsys)
	if !valid || got.LastFingerprint != state.LastFingerprint {
		t.Fatalf("state=%#v valid=%t", got, valid)
	}
	for path := range fsys.files {
		if len(path) >= 4 && path[len(path)-4:] == ".tmp" {
			t.Fatalf("temporary remains: %s", path)
		}
	}

	fsys.files["/state/request.json"] = []byte(`{"previous":true}`)
	writeErr := errors.New("write failure")
	badFS := failingWriteFS{FileSystem: fsys, err: writeErr}
	if err := WriteRestartRequest("/state/request.json", RestartRequest{At: "2026-11-03T00:00:00.000Z", Reason: "test", Cause: "wedge"}, badFS); !errors.Is(err, writeErr) {
		t.Fatalf("write error = %v", err)
	}
	if string(fsys.files["/state/request.json"]) != `{"previous":true}` {
		t.Fatal("failed atomic write replaced previous request")
	}
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, valid := spawnStamp(value); valid {
			t.Fatalf("non-finite JSON number %v accepted", value)
		}
	}

}
func TestStoreReadStateMissingAndRequestBoundary(t *testing.T) {
	fsys := newTestFS()
	fsys.files["/request"] = []byte(`{"at":"2026-11-03T00:00:00Z","reason":"boundary"}`)
	now := time.Date(2026, 11, 3, 0, 30, 0, 0, time.UTC)
	request, stale, err := ReadRestartRequest("/request", now, fsys)
	if err != nil || !stale || request != nil {
		t.Fatalf("request at 30 minute boundary = %#v,%t,%v", request, stale, err)
	}
	if _, valid := ReadState("/unreadable", failingReadFS{FileSystem: fsys, err: fs.ErrPermission}); valid {
		t.Fatal("unreadable state treated as missing")
	}
}

type failingReadFS struct {
	FileSystem
	err error
}

func (f failingReadFS) ReadFile(string) ([]byte, error) { return nil, f.err }
