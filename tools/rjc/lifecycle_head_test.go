package main

import (
	"strings"
	"testing"
)

func TestLifecycleStaleLocalHead(t *testing.T) {
	l := newLifecycleFixture(t)
	l.clear("agent", l.head, "main")
	l.f.commit(t, "later.txt")
	jjTest(t, l.f.repo, "bookmark", "move", l.bookmark, "--to", "@-")
	code, out := l.command(t, "note", l.bookmark, "--clear", "--reviewed", l.head)
	if code != 1 || !strings.Contains(out, "head-mismatch") || l.posts != 0 {
		t.Fatalf("%d %s", code, out)
	}
	meta := l.f.metadata[1]
	meta.Draft = false
	l.f.metadata[1] = meta
	code, out = l.command(t, "ready", l.bookmark)
	if code != 1 || !strings.Contains(out, "head-mismatch") || l.promotions != 0 {
		t.Fatalf("%d %s", code, out)
	}
}
