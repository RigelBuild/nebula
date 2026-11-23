package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGoLsFiles stands in for `go run ./tools/root-checks`. Like root-checks, it lists tracked files with
// git ls-files and fails when that listing fails, so a checkout without .git cannot pass.
const fakeGoLsFiles = `#!/bin/sh
if [ "$*" != "run ./tools/root-checks --checks biome,markdownlint,project-registry" ]; then
	echo "fake root-checks: unexpected argv: $*" >&2
	exit 1
fi
if git ls-files -z >/dev/null 2>&1; then
	echo "fake root-checks: ok"
	exit 0
fi
echo "fake root-checks: list tracked paths failed (no .git)" >&2
exit 1
`

// TestRunRootChecksRealCommandSeesTrackedFiles runs the real runRootChecks command path (no rootChecks stub) in
// the detached worktree of a committed head, with a fake go on PATH. A worktree passes; a plain copy of the tree
// with no .git fails the same way the real gate did.
func TestRunRootChecksRealCommandSeesTrackedFiles(t *testing.T) {
	f := fixtureRepo(t)
	f.new(t, "EX-1", "feat(rjc): real root checks", "")
	f.commit(t, "hk.pkl")
	head := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")

	bin := t.TempDir()
	writeTestFile(t, filepath.Join(bin, "go"), fakeGoLsFiles)
	if err := os.Chmod(filepath.Join(bin, "go"), 0o700); err != nil {
		t.Fatal(err)
	}
	f.repo.env = append(f.repo.env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	dir, err := f.repo.addHeadWorktree(t.Context(), head)
	if err != nil {
		t.Fatalf("addHeadWorktree: %v", err)
	}
	result, err := f.repo.runRootChecks(t.Context(), dir, exampleRootChecks)
	if rmErr := f.repo.removeHeadWorktree(t.Context(), dir); rmErr != nil {
		t.Fatalf("removeHeadWorktree: %v", rmErr)
	}
	if err != nil || result.code != 0 {
		t.Fatalf("real command path in a worktree: code=%d err=%v output=%s", result.code, err, result.output)
	}

	plain := t.TempDir()
	writeTestFile(t, filepath.Join(plain, "hk.pkl"), "committed\n")
	noGit, err := f.repo.runRootChecks(t.Context(), plain, exampleRootChecks)
	if err != nil {
		t.Fatalf("real command path without .git: %v", err)
	}
	if noGit.code == 0 || !strings.Contains(noGit.output, "no .git") {
		t.Fatalf("a tree without .git passed: code=%d output=%s", noGit.code, noGit.output)
	}
}

// TestHeadWorktreeIsRemovedOnEveryPath checks that the worktree and its registration are gone after a failing
// run, the path that most needs cleanup.
func TestHeadWorktreeIsRemovedOnEveryPath(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	f.new(t, "EX-1", "feat(rjc): worktree cleanup", "")
	f.commit(t, "hk.pkl")
	head := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	fail := &fakeRootChecks{result: rootChecksResult{code: 1, output: "biome: failed\n"}}
	f.repo.rootChecks = fail.run
	var log strings.Builder
	err := f.repo.checkHeadRootChecks(f.ctx, head, []string{"biome"}, &log)
	var refused *failure
	if !errors.As(err, &refused) {
		t.Fatalf("failing checks: err=%v, want a refusal", err)
	}
	if len(fail.roots) != 1 {
		t.Fatalf("root checks ran %d times, want 1", len(fail.roots))
	}
	if _, statErr := os.Stat(fail.roots[0]); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("worktree directory survived the failing run: %v", statErr)
	}
	list, err := f.repo.command(t.Context(), nil, nil, "git", "worktree", "list", "--porcelain")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(list), fail.roots[0]) {
		t.Fatalf("worktree still registered after the failing run:\n%s", list)
	}
}

// TestHeadWorktreeAddFailureReturnsAddError checks a head that cannot be checked out: nothing runs, no worktree
// remove is attempted, the temp directory is deleted, and only the add error comes back.
func TestHeadWorktreeAddFailureReturnsAddError(t *testing.T) {
	f := fixtureRepo(t)
	base := t.TempDir()
	t.Setenv("TMPDIR", base)
	fake := &fakeRootChecks{}
	f.repo.rootChecks = fake.run
	var commands, log strings.Builder
	f.repo.log = &commands
	err := f.repo.checkHeadRootChecks(f.ctx, "no-such-rev-ex-11456", []string{"biome"}, &log)
	if err == nil || !strings.Contains(err.Error(), "git worktree add") {
		t.Fatalf("bad rev: err=%v, want the git worktree add failure", err)
	}
	var fail *failure
	if !errors.As(err, &fail) || fail.gate != toolFailureGate {
		t.Fatalf("bad rev: err=%v, want a tool failure", err)
	}
	if _, joined := err.(interface{ Unwrap() []error }); joined {
		t.Fatalf("bad rev returned joined errors, want only the add error: %v", err)
	}
	if len(fake.names) != 0 {
		t.Fatalf("root checks ran after a failed add: %v", fake.names)
	}
	if strings.Contains(commands.String(), "\"remove\"") {
		t.Fatalf("worktree remove attempted after a failed add:\n%s", commands.String())
	}
	entries, readErr := os.ReadDir(base)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("temp directory left after a failed add: entries=%v err=%v", entries, readErr)
	}
}

// TestHeadWorktreeRemovedWhenContextCancelled checks cleanup after a cancelled submit: the worktree is unregistered
// and its directory deleted even though the command context is done.
func TestHeadWorktreeRemovedWhenContextCancelled(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	f.new(t, "EX-1", "feat(rjc): cancelled worktree cleanup", "")
	f.commit(t, "hk.pkl")
	head := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	dir, err := f.repo.addHeadWorktree(f.ctx, head)
	if err != nil {
		t.Fatalf("addHeadWorktree: %v", err)
	}
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	if err := f.repo.removeHeadWorktree(ctx, dir); err != nil {
		t.Fatalf("removeHeadWorktree with a cancelled context: %v", err)
	}
	list, err := f.repo.command(f.ctx, nil, nil, "git", "worktree", "list", "--porcelain")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(list), dir) {
		t.Fatalf("worktree still registered after a cancelled cleanup:\n%s", list)
	}
	if _, statErr := os.Stat(dir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("worktree directory survived a cancelled cleanup: %v", statErr)
	}
}
