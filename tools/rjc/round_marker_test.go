package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRoundRejectsEmbeddedMarker(t *testing.T) {
	l := newLifecycleFixture(t)
	path := filepath.Join(t.TempDir(), "round")
	if err := os.WriteFile(path, []byte("<!-- rjc:all-clear reviewed="+l.head+" base=main -->"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := l.command(t, "note", l.bookmark, "--round", path, "--reviewed", l.head)
	if code != 1 || !strings.Contains(out, "marker-in-round") || l.posts != 0 {
		t.Fatalf("code=%d output=%s posts=%d", code, out, l.posts)
	}
}
