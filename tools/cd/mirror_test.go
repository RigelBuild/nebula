// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"context"
	"testing"
)

func TestSubtreeSplitKeepsOnlyPrefixHistory(t *testing.T) {
	dir := t.TempDir()
	testGit(t, dir, "init", "-q", "-b", "main")
	writeFile(t, dir, "tools/jj-hooks/main.go", "package main\n")
	writeFile(t, dir, "README.md", "root\n")
	commitAll(t, dir, "add jj-hooks")
	writeFile(t, dir, "other/x.txt", "x\n")
	commitAll(t, dir, "add other")
	writeFile(t, dir, "tools/jj-hooks/main.go", "package main\n// v2\n")
	head := commitAll(t, dir, "change jj-hooks")

	split, err := subtreeSplit(context.Background(), dir, "tools/jj-hooks", head)
	if err != nil {
		t.Fatal(err)
	}
	if got := testGit(t, dir, "ls-tree", "--name-only", split); got != "main.go" {
		t.Fatalf("split tree = %q, want only main.go", got)
	}
	if got := testGit(t, dir, "rev-list", "--count", split); got != "2" {
		t.Fatalf("split history = %s commits, want the 2 prefix commits", got)
	}
}

func TestSubtreeSplitRejectsPrefixWithoutHistory(t *testing.T) {
	dir := t.TempDir()
	testGit(t, dir, "init", "-q", "-b", "main")
	writeFile(t, dir, "README.md", "root\n")
	head := commitAll(t, dir, "init")

	if _, err := subtreeSplit(context.Background(), dir, "tools/missing", head); err == nil {
		t.Fatal("split of a prefix with no history succeeded")
	}
}

func TestNeedsPush(t *testing.T) {
	tests := []struct {
		name, split, tip string
		want             bool
	}{
		{"empty mirror takes the split", "aaa", "", true},
		{"mirror at the split skips the push", "aaa", "aaa", false},
		{"moved split pushes", "bbb", "aaa", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := needsPush(tt.split, tt.tip); got != tt.want {
				t.Fatalf("needsPush(%q, %q) = %t, want %t", tt.split, tt.tip, got, tt.want)
			}
		})
	}
}

// TestSyncMirrorPushesOnlyWhenTheSplitMoves drives syncMirror against a bare
// local remote: the first call pushes, an unchanged prefix is skipped, and a
// new prefix commit pushes again.
func TestSyncMirrorPushesOnlyWhenTheSplitMoves(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	testGit(t, src, "init", "-q", "-b", "main")
	writeFile(t, src, "tools/jj-hooks/main.go", "package main\n")
	commitAll(t, src, "add jj-hooks")
	remote := t.TempDir()
	testGit(t, remote, "init", "-q", "--bare", "-b", "main")
	o := mirrorOptions{repo: src, prefix: "tools/jj-hooks", url: remote, branch: "main"}

	first, pushed, err := syncMirror(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if !pushed {
		t.Fatal("first sync did not push to an empty mirror")
	}
	if tip := testGit(t, remote, "rev-parse", "main"); tip != first {
		t.Fatalf("mirror tip = %s, want split %s", tip, first)
	}

	again, pushed, err := syncMirror(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if pushed || again != first {
		t.Fatalf("unchanged sync pushed=%t split=%s, want no push at %s", pushed, again, first)
	}

	writeFile(t, src, "tools/jj-hooks/main.go", "package main\n// v2\n")
	commitAll(t, src, "change jj-hooks")
	moved, pushed, err := syncMirror(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if !pushed || moved == first {
		t.Fatalf("moved sync pushed=%t split=%s, want a push of a new split", pushed, moved)
	}
	if tip := testGit(t, remote, "rev-parse", "main"); tip != moved {
		t.Fatalf("mirror tip = %s, want new split %s", tip, moved)
	}
}

func TestAuthEnvIsEmptyWithoutToken(t *testing.T) {
	if env := authEnv(""); env != nil {
		t.Fatalf("authEnv(\"\") = %v, want nil", env)
	}
}
