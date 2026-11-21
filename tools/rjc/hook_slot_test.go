package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hookSlotGit runs git in dir with a fixed identity and no user config, and fails the test on error.
func hookSlotGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestSSHHookBodyDiscardsTrackedEditsInReusedSlot proves a reused hook slot with a modified tracked file still checks out
// the target. An earlier run left .pr-body.md dirty; the checkout aborted on it before clean ran, so every later submit failed.
func TestSSHHookBodyDiscardsTrackedEditsInReusedSlot(t *testing.T) {
	repo := t.TempDir()
	slot := filepath.Join(t.TempDir(), "w0")
	hookSlotGit(t, repo, "init", "-q")
	writeTestFile(t, filepath.Join(repo, "hk.pkl"), "config\n")
	writeTestFile(t, filepath.Join(repo, ".pr-body.md"), "committed-a\n")
	hookSlotGit(t, repo, "add", "-A")
	hookSlotGit(t, repo, "commit", "-q", "-m", "a")
	base := hookSlotGit(t, repo, "rev-parse", "HEAD")
	hookSlotGit(t, repo, "worktree", "add", "-q", "--detach", slot, base)
	writeTestFile(t, filepath.Join(slot, ".pr-body.md"), "dirty\n")
	writeTestFile(t, filepath.Join(repo, ".pr-body.md"), "committed-b\n")
	hookSlotGit(t, repo, "commit", "-q", "-am", "b")
	head := hookSlotGit(t, repo, "rev-parse", "HEAD")
	cmd := exec.CommandContext(t.Context(), "sh", "-c", sshHookBody, "sh", "base", head, "0", slot, t.TempDir(), repo, "unused", "lefthook", "hk.pkl", "30s")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("checkout over a modified tracked file: %v: %s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(slot, ".pr-body.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "committed-b\n" {
		t.Fatalf("slot kept the local edit: %q", got)
	}
}

// TestSSHHookBodyClearsStaleIndexLock proves a slot whose index.lock a killed run left behind still checks out the target.
// The flock holder is the only git process that can own the slot, so the lock is stale and the body removes it.
func TestSSHHookBodyClearsStaleIndexLock(t *testing.T) {
	repo := t.TempDir()
	slot := filepath.Join(t.TempDir(), "w0")
	hookSlotGit(t, repo, "init", "-q")
	writeTestFile(t, filepath.Join(repo, "hk.pkl"), "config\n")
	hookSlotGit(t, repo, "add", "-A")
	hookSlotGit(t, repo, "commit", "-q", "-m", "a")
	base := hookSlotGit(t, repo, "rev-parse", "HEAD")
	hookSlotGit(t, repo, "worktree", "add", "-q", "--detach", slot, base)
	writeTestFile(t, filepath.Join(repo, "tracked.txt"), "from-target\n")
	hookSlotGit(t, repo, "add", "-A")
	hookSlotGit(t, repo, "commit", "-q", "-m", "b")
	head := hookSlotGit(t, repo, "rev-parse", "HEAD")
	writeTestFile(t, filepath.Join(hookSlotGit(t, slot, "rev-parse", "--absolute-git-dir"), "index.lock"), "")
	cmd := exec.CommandContext(t.Context(), "sh", "-c", sshHookBody, "sh", "base", head, "0", slot, t.TempDir(), repo, "unused", "lefthook", "hk.pkl", "30s")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("checkout past a stale index.lock: %v: %s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(slot, "tracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "from-target\n" {
		t.Fatalf("slot did not check out the target: %q", got)
	}
}

// TestSSHHookBodyOverwritesUntrackedFileTheTargetTracks proves an untracked file at a path the target commit tracks does not
// abort the checkout. Before -f, git refused with "untracked working tree files would be overwritten by checkout".
func TestSSHHookBodyOverwritesUntrackedFileTheTargetTracks(t *testing.T) {
	repo := t.TempDir()
	slot := filepath.Join(t.TempDir(), "w0")
	hookSlotGit(t, repo, "init", "-q")
	writeTestFile(t, filepath.Join(repo, "hk.pkl"), "config\n")
	hookSlotGit(t, repo, "add", "-A")
	hookSlotGit(t, repo, "commit", "-q", "-m", "a")
	base := hookSlotGit(t, repo, "rev-parse", "HEAD")
	hookSlotGit(t, repo, "worktree", "add", "-q", "--detach", slot, base)
	writeTestFile(t, filepath.Join(repo, "tracked.txt"), "from-target\n")
	hookSlotGit(t, repo, "add", "-A")
	hookSlotGit(t, repo, "commit", "-q", "-m", "b")
	head := hookSlotGit(t, repo, "rev-parse", "HEAD")
	writeTestFile(t, filepath.Join(slot, "tracked.txt"), "untracked\n")
	cmd := exec.CommandContext(t.Context(), "sh", "-c", sshHookBody, "sh", "base", head, "0", slot, t.TempDir(), repo, "unused", "lefthook", "hk.pkl", "30s")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("checkout over an untracked file the target tracks: %v: %s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(slot, "tracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "from-target\n" {
		t.Fatalf("slot kept the untracked file: %q", got)
	}
}

// TestSSHHookBodyClearsStaleHeadLock proves a slot whose HEAD.lock a killed run left behind still checks out the target.
// The flock holder owns the slot's private HEAD, so the lock is stale and the body removes it.
func TestSSHHookBodyClearsStaleHeadLock(t *testing.T) {
	repo := t.TempDir()
	slot := filepath.Join(t.TempDir(), "w0")
	hookSlotGit(t, repo, "init", "-q")
	writeTestFile(t, filepath.Join(repo, "hk.pkl"), "config\n")
	hookSlotGit(t, repo, "add", "-A")
	hookSlotGit(t, repo, "commit", "-q", "-m", "a")
	base := hookSlotGit(t, repo, "rev-parse", "HEAD")
	hookSlotGit(t, repo, "worktree", "add", "-q", "--detach", slot, base)
	writeTestFile(t, filepath.Join(repo, "tracked.txt"), "from-target\n")
	hookSlotGit(t, repo, "add", "-A")
	hookSlotGit(t, repo, "commit", "-q", "-m", "b")
	head := hookSlotGit(t, repo, "rev-parse", "HEAD")
	writeTestFile(t, filepath.Join(hookSlotGit(t, slot, "rev-parse", "--absolute-git-dir"), "HEAD.lock"), "")
	cmd := exec.CommandContext(t.Context(), "sh", "-c", sshHookBody, "sh", "base", head, "0", slot, t.TempDir(), repo, "unused", "lefthook", "hk.pkl", "30s")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("checkout past a stale HEAD.lock: %v: %s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(slot, "tracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "from-target\n" {
		t.Fatalf("slot did not check out the target: %q", got)
	}
}
