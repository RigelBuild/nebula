// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"bytes"
	"context"
	"strings"
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

// newSyncSource returns a repo with one commit that touches tools/jj-hooks.
func newSyncSource(t *testing.T) (src string) {
	t.Helper()
	src = t.TempDir()
	testGit(t, src, "init", "-q", "-b", "main")
	writeFile(t, src, "tools/jj-hooks/main.go", "package main\n")
	commitAll(t, src, "add jj-hooks")
	return src
}

// TestSyncMirrorTakesTheSplitOnlyWhenItExtendsTheTip drives syncMirror against a
// bare local remote with three tips: unrelated history is skipped with a warning,
// an ancestor tip is fast-forwarded, and an equal tip is a no-op.
func TestSyncMirrorTakesTheSplitOnlyWhenItExtendsTheTip(t *testing.T) {
	ctx := context.Background()

	t.Run("unrelated history skips the push with a warning", func(t *testing.T) {
		src := newSyncSource(t)
		remote := t.TempDir()
		testGit(t, remote, "init", "-q", "--bare", "-b", "main")
		other := t.TempDir()
		testGit(t, other, "init", "-q", "-b", "main")
		writeFile(t, other, "unrelated.txt", "u\n")
		unrelatedTip := commitAll(t, other, "unrelated history")
		testGit(t, other, "push", "-q", remote, "main")
		o := mirrorOptions{repo: src, prefix: "tools/jj-hooks", url: remote, branch: "main"}

		var warn bytes.Buffer
		split, state, err := syncMirror(ctx, o, &warn)
		if err != nil {
			t.Fatal(err)
		}
		if state != mirrorSkipped {
			t.Fatalf("state = %s, want %s", state, mirrorSkipped)
		}
		if tip := testGit(t, remote, "rev-parse", "main"); tip != unrelatedTip {
			t.Fatalf("mirror tip = %s, want the unrelated tip %s left alone (split %s)", tip, unrelatedTip, split)
		}
		if !strings.Contains(warn.String(), "RIG-10830") {
			t.Fatalf("warning = %q, want it to name RIG-10830", warn.String())
		}
	})

	t.Run("ancestor tip is fast-forwarded", func(t *testing.T) {
		src := newSyncSource(t)
		remote := t.TempDir()
		testGit(t, remote, "init", "-q", "--bare", "-b", "main")
		o := mirrorOptions{repo: src, prefix: "tools/jj-hooks", url: remote, branch: "main"}
		first, _, err := syncMirror(ctx, o, &bytes.Buffer{})
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, src, "tools/jj-hooks/main.go", "package main\n// v2\n")
		commitAll(t, src, "change jj-hooks")

		var warn bytes.Buffer
		moved, state, err := syncMirror(ctx, o, &warn)
		if err != nil {
			t.Fatal(err)
		}
		if state != mirrorPushed || moved == first {
			t.Fatalf("state = %s split = %s, want a push of a new split after %s", state, moved, first)
		}
		if tip := testGit(t, remote, "rev-parse", "main"); tip != moved {
			t.Fatalf("mirror tip = %s, want new split %s", tip, moved)
		}
		if warn.Len() != 0 {
			t.Fatalf("fast-forward wrote a warning: %q", warn.String())
		}
	})

	t.Run("equal tip is a no-op", func(t *testing.T) {
		src := newSyncSource(t)
		remote := t.TempDir()
		testGit(t, remote, "init", "-q", "--bare", "-b", "main")
		o := mirrorOptions{repo: src, prefix: "tools/jj-hooks", url: remote, branch: "main"}
		first, _, err := syncMirror(ctx, o, &bytes.Buffer{})
		if err != nil {
			t.Fatal(err)
		}

		again, state, err := syncMirror(ctx, o, &bytes.Buffer{})
		if err != nil {
			t.Fatal(err)
		}
		if state != mirrorUnchanged || again != first {
			t.Fatalf("state = %s split = %s, want unchanged at %s", state, again, first)
		}
	})
}

func TestAuthEnvIsEmptyWithoutToken(t *testing.T) {
	if env := authEnv(""); env != nil {
		t.Fatalf("authEnv(\"\") = %v, want nil", env)
	}
}

// TestAuthEnvPassesTokenThroughGitConfigOnly checks L2 and M2: a non-empty token
// reaches git as a GIT_CONFIG_* header and never as a command argument.
func TestAuthEnvPassesTokenThroughGitConfigOnly(t *testing.T) {
	const token = "ghs_exampletoken"
	env := authEnv(token)
	want := []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
		"GIT_CONFIG_VALUE_0=AUTHORIZATION: basic eC1hY2Nlc3MtdG9rZW46Z2hzX2V4YW1wbGV0b2tlbg==",
	}
	if len(env) != len(want) {
		t.Fatalf("authEnv(token) = %v, want %v", env, want)
	}
	for i := range want {
		if env[i] != want[i] {
			t.Fatalf("authEnv(token)[%d] = %q, want %q", i, env[i], want[i])
		}
	}
}
