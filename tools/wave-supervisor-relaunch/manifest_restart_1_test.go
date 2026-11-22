package main

import (
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/relaunch"
)

func TestRestartRequestStorage(t *testing.T) {
	now := time.Date(2026, time.November, 3, 0, 0, 0, 0, time.UTC)

	t.Run("defaults a missing cause to wedge", func(t *testing.T) {
		fsys := newMemoryFS()
		fsys.files["/restart"] = []byte(`{"at":"2026-11-03T00:00:00.000Z","reason":"test"}`)
		request, stale, err := relaunch.ReadRestartRequest("/restart", now, fsys)
		want := &relaunch.RestartRequest{At: "2026-11-03T00:00:00.000Z", Reason: "test", Cause: "wedge"}
		if err != nil || stale || request == nil || *request != *want {
			t.Fatalf("ReadRestartRequest() = %#v, %v, %v, want %#v, false, nil", request, stale, err, want)
		}
	})

	t.Run("reads rss identity and rejects missing or unknown causes as stale", func(t *testing.T) {
		fsys := newMemoryFS()
		fsys.files["/restart"] = []byte(`{"at":"2026-11-03T00:00:00.000Z","reason":"rss","cause":"rss","identity":"123:456"}`)
		request, stale, err := relaunch.ReadRestartRequest("/restart", now, fsys)
		if err != nil || stale || request == nil || request.Cause != "rss" || request.Identity != "123:456" {
			t.Fatalf("ReadRestartRequest() = %#v, %v, %v, want rss identity 123:456", request, stale, err)
		}
		fsys.files["/restart"] = []byte(`{"at":"2026-11-03T00:00:00.000Z","reason":"rss","cause":"rss"}`)
		request, stale, err = relaunch.ReadRestartRequest("/restart", now, fsys)
		if err != nil || !stale || request != nil {
			t.Fatalf("missing identity result = %#v, %v, %v, want nil, true, nil", request, stale, err)
		}
		fsys.files["/restart"] = []byte(`{"at":"2026-11-03T00:00:00.000Z","reason":"test","cause":"other"}`)
		request, stale, err = relaunch.ReadRestartRequest("/restart", now, fsys)
		if err != nil || !stale || request != nil {
			t.Fatalf("unknown cause result = %#v, %v, %v, want nil, true, nil", request, stale, err)
		}
	})
}
