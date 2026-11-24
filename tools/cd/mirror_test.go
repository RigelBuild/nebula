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

func newSyncMirrorWithUnrelatedTip(t *testing.T) (string, string, mirrorOptions, string) {
	t.Helper()
	src := newSyncSource(t)
	remote := t.TempDir()
	testGit(t, remote, "init", "-q", "--bare", "-b", "main")
	other := t.TempDir()
	testGit(t, other, "init", "-q", "-b", "main")
	writeFile(t, other, "unrelated.txt", "u\n")
	unrelatedTip := commitAll(t, other, "unrelated history")
	testGit(t, other, "push", "-q", remote, "main")
	o := mirrorOptions{repo: src, prefix: "tools/jj-hooks", url: remote, branch: "main"}
	return src, remote, o, unrelatedTip
}

func TestSyncMirrorBridgesUnrelatedHistory(t *testing.T) {
	ctx := context.Background()
	src, remote, o, unrelatedTip := newSyncMirrorWithUnrelatedTip(t)
	var warn bytes.Buffer
	split, state, err := syncMirror(ctx, o, &warn)
	if err != nil {
		t.Fatal(err)
	}
	if state != mirrorPushed {
		t.Fatalf("state = %s, want %s", state, mirrorPushed)
	}
	if warn.Len() == 0 || strings.Contains(warn.String(), "RIG-") {
		t.Fatalf("warning = %q, want an issue-free warning", warn.String())
	}

	bridge := testGit(t, remote, "rev-parse", "main")
	parents := strings.Fields(testGit(t, src, "rev-list", "--parents", "-n", "1", bridge))
	if len(parents) != 3 || parents[1] != unrelatedTip || parents[2] != split {
		t.Fatalf("bridge parents = %v, want %s and %s", parents, unrelatedTip, split)
	}
	if message := testGit(t, src, "show", "-s", "--format=%s", bridge); message != "Merge history into the nebula subtree split" {
		t.Fatalf("bridge message = %q", message)
	}
	identity := testGit(t, src, "show", "-s", "--format=%an %ae %cn %ce", bridge)
	wantIdentity := "rigelbuild-nebula-release[bot] rigelbuild-nebula-release[bot]@users.noreply.github.com " +
		"rigelbuild-nebula-release[bot] rigelbuild-nebula-release[bot]@users.noreply.github.com"
	if identity != wantIdentity {
		t.Fatalf("bridge identity = %q, want %q", identity, wantIdentity)
	}
	for _, ancestor := range []string{unrelatedTip, split} {
		reachable, err := isAncestor(ctx, src, ancestor, bridge)
		if err != nil {
			t.Fatal(err)
		}
		if !reachable {
			t.Fatalf("bridge %s does not preserve history from %s", bridge, ancestor)
		}
	}
	if tree := testGit(t, src, "rev-parse", bridge+"^{tree}"); tree != testGit(t, src, "rev-parse", split+"^{tree}") {
		t.Fatalf("bridge tree = %s, want split tree %s", tree, testGit(t, src, "rev-parse", split+"^{tree}"))
	}
}

func TestSyncMirrorRejectsForeignMergeAsContinuation(t *testing.T) {
	ctx := context.Background()
	src, remote, o, unrelatedTip := newSyncMirrorWithUnrelatedTip(t)
	split, err := subtreeSplit(ctx, src, o.prefix, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gitRun(ctx, src, nil, nil, "fetch", "-q", remote, "refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	foreignMerge := testGit(
		t,
		src,
		"commit-tree",
		unrelatedTip+"^{tree}",
		"-p",
		unrelatedTip,
		"-p",
		split,
		"-m",
		"foreign merge",
	)
	testGit(t, src, "push", remote, foreignMerge+":refs/heads/main")

	var warn bytes.Buffer
	moved, state, err := syncMirror(ctx, o, &warn)
	if err != nil {
		t.Fatal(err)
	}
	if state != mirrorPushed || moved != split {
		t.Fatalf("state = %s split = %s, want pushed split %s", state, moved, split)
	}
	if warn.Len() == 0 {
		t.Fatal("foreign merge was accepted as a bridge continuation without warning")
	}
	newTip := testGit(t, remote, "rev-parse", "main")
	parents := strings.Fields(testGit(t, src, "rev-list", "--parents", "-n", "1", newTip))
	if len(parents) != 3 || parents[1] != foreignMerge || parents[2] != split {
		t.Fatalf("replacement bridge parents = %v, want %s and %s", parents, foreignMerge, split)
	}
}

func TestSyncMirrorAdvancesBridgeMerge(t *testing.T) {
	ctx := context.Background()
	src, remote, o, _ := newSyncMirrorWithUnrelatedTip(t)
	firstSplit, state, err := syncMirror(ctx, o, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if state != mirrorPushed {
		t.Fatalf("first state = %s, want %s", state, mirrorPushed)
	}
	firstTip := testGit(t, remote, "rev-parse", "main")
	writeFile(t, src, "tools/jj-hooks/main.go", "package main\n// v2\n")
	commitAll(t, src, "change jj-hooks")

	var warn bytes.Buffer
	secondSplit, state, err := syncMirror(ctx, o, &warn)
	if err != nil {
		t.Fatal(err)
	}
	if state != mirrorPushed || secondSplit == firstSplit {
		t.Fatalf("state = %s split = %s, want a push of a new split after %s", state, secondSplit, firstSplit)
	}
	secondTip := testGit(t, remote, "rev-parse", "main")
	if secondTip == secondSplit {
		t.Fatal("second run replaced the bridge merge with the split")
	}
	parents := strings.Fields(testGit(t, src, "rev-list", "--parents", "-n", "1", secondTip))
	if len(parents) != 3 || parents[1] != firstTip || parents[2] != secondSplit {
		t.Fatalf("second bridge parents = %v, want %s and %s", parents, firstTip, secondSplit)
	}
	fastForward, err := isAncestor(ctx, src, firstTip, secondTip)
	if err != nil {
		t.Fatal(err)
	}
	if !fastForward {
		t.Fatalf("second push did not advance from mirror tip %s", firstTip)
	}
	if tree := testGit(t, src, "rev-parse", secondTip+"^{tree}"); tree != testGit(t, src, "rev-parse", secondSplit+"^{tree}") {
		t.Fatalf("second bridge tree = %s, want split tree", tree)
	}
	if warn.Len() != 0 {
		t.Fatalf("recognized bridge continuation wrote a warning: %q", warn.String())
	}
}

func TestSyncMirrorRepeatedBridgeSplitIsUnchanged(t *testing.T) {
	ctx := context.Background()
	_, remote, o, _ := newSyncMirrorWithUnrelatedTip(t)
	_, state, err := syncMirror(ctx, o, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if state != mirrorPushed {
		t.Fatalf("first state = %s, want %s", state, mirrorPushed)
	}
	firstTip := testGit(t, remote, "rev-parse", "main")

	_, state, err = syncMirror(ctx, o, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if state != mirrorUnchanged {
		t.Fatalf("second state = %s, want %s", state, mirrorUnchanged)
	}
	if tip := testGit(t, remote, "rev-parse", "main"); tip != firstTip {
		t.Fatalf("mirror tip = %s after unchanged run, want %s", tip, firstTip)
	}
}

func TestSyncMirrorFastForwardsAncestorTip(t *testing.T) {
	ctx := context.Background()
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
	fastForward, err := isAncestor(ctx, src, first, moved)
	if err != nil {
		t.Fatal(err)
	}
	if !fastForward {
		t.Fatalf("new split %s is not a fast-forward from %s", moved, first)
	}
	if warn.Len() != 0 {
		t.Fatalf("fast-forward wrote a warning: %q", warn.String())
	}
}

func TestSyncMirrorEqualTipIsNoOp(t *testing.T) {
	ctx := context.Background()
	src := newSyncSource(t)
	remote := t.TempDir()
	testGit(t, remote, "init", "-q", "--bare", "-b", "main")
	o := mirrorOptions{repo: src, prefix: "tools/jj-hooks", url: remote, branch: "main"}
	first, state, err := syncMirror(ctx, o, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if state != mirrorPushed {
		t.Fatalf("empty mirror state = %s, want %s", state, mirrorPushed)
	}
	if tip := testGit(t, remote, "rev-parse", "main"); tip != first {
		t.Fatalf("empty mirror tip = %s, want split %s", tip, first)
	}
	again, state, err := syncMirror(ctx, o, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if state != mirrorUnchanged || again != first {
		t.Fatalf("state = %s split = %s, want unchanged at %s", state, again, first)
	}
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
