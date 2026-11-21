package main

import (
	"testing"
	"time"
)

func TestClassifyWorkspace(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	idle := 2 * 24 * time.Hour
	old := now.Add(-idle - time.Hour)
	recent := now.Add(-idle + time.Hour)
	boundary := now.Add(-idle)

	cases := []struct {
		name string
		ws   workspace
		want verdict
	}{
		{
			name: "old clean fully pushed is deleted",
			ws:   workspace{LastActive: old, Clean: true},
			want: verdictDelete,
		},
		{
			name: "idle exactly at the limit is deleted",
			ws:   workspace{LastActive: boundary, Clean: true},
			want: verdictDelete,
		},
		{
			name: "old clean with an unpushed change is reported, never deleted",
			ws:   workspace{LastActive: old, Clean: true, Unpushed: []string{"abc123"}},
			want: verdictReport,
		},
		{
			name: "old dirty with nothing unpushed is reported, never deleted",
			ws:   workspace{LastActive: old, Clean: false},
			want: verdictReport,
		},
		{
			name: "old dirty with an unpushed change is reported",
			ws:   workspace{LastActive: old, Clean: false, Unpushed: []string{"abc123"}},
			want: verdictReport,
		},
		{
			name: "recent clean workspace is kept",
			ws:   workspace{LastActive: recent, Clean: true},
			want: verdictRecent,
		},
		{
			name: "recent dirty workspace is kept, not reported",
			ws:   workspace{LastActive: recent, Clean: false, Unpushed: []string{"abc123"}},
			want: verdictRecent,
		},
		{
			name: "old clean workspace with a live cwd is skipped",
			ws:   workspace{LastActive: old, Clean: true, LiveCwd: true},
			want: verdictLive,
		},
		{
			name: "live cwd wins over unpushed and dirty state",
			ws:   workspace{LastActive: old, Clean: false, Unpushed: []string{"abc123"}, LiveCwd: true},
			want: verdictLive,
		},
		{
			name: "live cwd wins over recency",
			ws:   workspace{LastActive: recent, Clean: true, LiveCwd: true},
			want: verdictLive,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := classify(tc.ws, now, idle); got != tc.want {
				t.Fatalf("classify() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWithin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		path string
		root string
		want bool
	}{
		{name: "root itself", path: "/w/ws", root: "/w/ws", want: true},
		{name: "child of root", path: "/w/ws/src", root: "/w/ws", want: true},
		{name: "sibling sharing a prefix", path: "/w/ws-2", root: "/w/ws", want: false},
		{name: "parent of root", path: "/w", root: "/w/ws", want: false},
		{name: "unrelated", path: "/tmp", root: "/w/ws", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := within(tc.path, tc.root); got != tc.want {
				t.Fatalf("within(%q, %q) = %t, want %t", tc.path, tc.root, got, tc.want)
			}
		})
	}
}
