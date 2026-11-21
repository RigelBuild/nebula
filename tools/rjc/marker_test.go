package main

import (
	"strings"
	"testing"
)

func TestMarkerRoundTrip(t *testing.T) {
	sha := strings.Repeat("a", 40)
	want := Marker{Kind: "all-clear", Reviewed: sha, Base: "main", Author: "alice", URL: "https://example.test/comment"}
	got := ParseMarkers("review\n"+FormatMarker(want), want.Author, want.URL)
	if len(got) != 1 || got[0] != want {
		t.Fatalf("markers = %#v, want %#v", got, want)
	}
}

func TestMarkerRejectsInvalid(t *testing.T) {
	for _, body := range []string{
		"<!-- rjc:all-clear reviewed=bad base=main -->",
		"<!-- rjc:unknown reviewed=" + strings.Repeat("a", 40) + " base=main -->",
		"<!-- rjc:round reviewed=" + strings.Repeat("A", 40) + " base=main -->",
	} {
		if got := ParseMarkers(body, "alice", "url"); len(got) != 0 {
			t.Errorf("accepted %q: %#v", body, got)
		}
	}
}
