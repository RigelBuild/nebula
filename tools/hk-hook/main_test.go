package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	stubMark = "stub-hk:"
	// asMainEnv makes the test binary run the installer, so each case spawns it
	// with its own cwd and env, as the TypeScript tests did.
	asMainEnv = "HK_HOOK_AS_MAIN"
	// wantCommand is the hook command the TypeScript installer wrote, byte for byte.
	wantCommand = `test "${HK:-1}" = "0" || direnv exec . hk run pre-push --from-hook "$@"`
)

func TestMain(m *testing.M) {
	if os.Getenv(asMainEnv) == "1" {
		os.Exit(run(context.Background(), os.Stderr))
	}
	os.Exit(m.Run())
}

// gitEnv isolates git and jj from the host: no system or global config, no inherited GIT_DIR.
func gitEnv(home string, extra ...string) []string {
	env := make([]string, 0, 4+len(extra))
	env = append(env,
		"HOME="+home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"PATH="+os.Getenv("PATH"),
	)
	return append(env, extra...)
}

// runCmd runs name in dir under env and returns stdout, stderr and the exit code.
func runCmd(t *testing.T, dir string, env []string, name string, args ...string) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	command := exec.CommandContext(t.Context(), name, args...)
	command.Dir = dir
	command.Env = env
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err == nil {
		return stdout.String(), stderr.String(), 0
	}
	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if !ok {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return stdout.String(), stderr.String(), exitErr.ExitCode()
}

// mustRun runs name and fails the test unless it exits 0. It returns stdout.
func mustRun(t *testing.T, dir string, env []string, name string, args ...string) string {
	t.Helper()
	stdout, stderr, code := runCmd(t, dir, env, name, args...)
	if code != 0 {
		t.Fatalf("%s %v exited %d: %s", name, args, code, stderr)
	}
	return stdout
}

// installer runs the installer in dir under env, as the devenv shell hook does.
func installer(t *testing.T, dir string, env []string) (string, int) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	_, stderr, code := runCmd(t, dir, slices.Concat(env, []string{asMainEnv + "=1"}), self)
	return stderr, code
}

// installHook runs the installer in repo and fails the test unless it exits 0.
func installHook(t *testing.T, repo string, env []string) {
	t.Helper()
	if stderr, code := installer(t, repo, env); code != 0 {
		t.Fatalf("install exit = %d: %s", code, stderr)
	}
}

// newRepo creates a bare remote and a main-branch clone with one commit.
func newRepo(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	env := gitEnv(root)
	mustRun(t, root, env, "git", "init", "-q", "--bare", filepath.Join(root, "remote.git"))
	mustRun(t, root, env, "git", "init", "-q", "-b", "main", repo)
	mustRun(t, repo, env, "git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "x")
	mustRun(t, repo, env, "git", "remote", "add", "origin", filepath.Join(root, "remote.git"))
	return root, repo
}

// writeStub writes an executable sh script into dir.
func writeStub(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
}

// shellEnv is a push from a shell outside the devenv: PATH holds git, sh and a stub direnv,
// which adds a stub hk to PATH as the real direnv does. The stub hk logs its argv to stderr.
func shellEnv(t *testing.T, root string) []string {
	t.Helper()
	bin := filepath.Join(root, "bin")
	devenv := filepath.Join(root, "devenv-bin")
	for _, dir := range []string{bin, devenv} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, tool := range []string{"git", "sh"} {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	writeStub(t, bin, "direnv", "[ \"$1\" = exec ] || exit 2\nshift 2\nPATH=\""+devenv+":$PATH\" exec \"$@\"")
	writeStub(t, devenv, "hk", "echo \""+stubMark+" $*\" >&2\nexit \"${HK_STUB_RC:-0}\"")
	return []string{"HOME=" + root, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "PATH=" + bin}
}

// hkRuns returns the stderr lines the stub hk logged.
func hkRuns(stderr string) []string {
	var runs []string
	for line := range strings.SplitSeq(stderr, "\n") {
		if strings.HasPrefix(line, stubMark) {
			runs = append(runs, line)
		}
	}
	return runs
}

// TestInstallWritesHookSettings pins the two config keys and their exact values.
func TestInstallWritesHookSettings(t *testing.T) {
	root, repo := newRepo(t)
	env := gitEnv(root)
	installHook(t, repo, env)
	command := mustRun(t, repo, env, "git", "config", "--get", "hook.hk-pre-push.command")
	if got := strings.TrimSuffix(command, "\n"); got != wantCommand {
		t.Fatalf("hook command = %q, want %q", got, wantCommand)
	}
	event := mustRun(t, repo, env, "git", "config", "--get", "hook.hk-pre-push.event")
	if got := strings.TrimSuffix(event, "\n"); got != "pre-push" {
		t.Fatalf("hook event = %q, want %q", got, "pre-push")
	}
}

// TestPrePushRunsGate pushes from a shell without hk or moon on PATH.
func TestPrePushRunsGate(t *testing.T) {
	root, repo := newRepo(t)
	push := shellEnv(t, root)
	installHook(t, repo, gitEnv(root))
	_, stderr, code := runCmd(t, repo, push, "git", "push", "-q", "origin", "main")
	if strings.Contains(stderr, "command not found") {
		t.Fatalf("stderr contains command not found: %q", stderr)
	}
	if code != 0 {
		t.Fatalf("push exit = %d, stderr %q", code, stderr)
	}
	runs := hkRuns(stderr)
	if len(runs) != 1 || !strings.HasPrefix(runs[0], stubMark+" run pre-push --from-hook origin ") {
		t.Fatalf("hk runs = %q, want one 'run pre-push --from-hook origin' line", runs)
	}
}

// TestPrePushFailingGateBlocksPush checks that a failing gate keeps the push off the remote.
func TestPrePushFailingGateBlocksPush(t *testing.T) {
	root, repo := newRepo(t)
	push := shellEnv(t, root)
	installHook(t, repo, gitEnv(root))
	failing := slices.Concat(push, []string{"HK_STUB_RC=1"})
	if _, _, code := runCmd(t, repo, failing, "git", "push", "-q", "origin", "main"); code == 0 {
		t.Fatal("push succeeded despite a failing gate")
	}
	remote := mustRun(t, repo, gitEnv(root), "git", "ls-remote", "origin", "main")
	if remote != "" {
		t.Fatalf("ls-remote = %q, want empty: the push must not land", remote)
	}
}

// TestPrePushHKZeroSkipsGate checks that HK=0 pushes without running the gate.
func TestPrePushHKZeroSkipsGate(t *testing.T) {
	root, repo := newRepo(t)
	push := shellEnv(t, root)
	installHook(t, repo, gitEnv(root))
	skip := slices.Concat(push, []string{"HK=0", "HK_STUB_RC=1"})
	_, stderr, code := runCmd(t, repo, skip, "git", "push", "-q", "origin", "main")
	if code != 0 {
		t.Fatalf("push exit = %d, want 0 with HK=0; stderr %q", code, stderr)
	}
	if runs := hkRuns(stderr); len(runs) != 0 {
		t.Fatalf("hk runs = %q, want none", runs)
	}
}

// TestInstallReinstallKeepsOneCommand checks that a second install replaces the hook command.
func TestInstallReinstallKeepsOneCommand(t *testing.T) {
	root, repo := newRepo(t)
	env := gitEnv(root)
	installHook(t, repo, env)
	installHook(t, repo, env)
	stdout := mustRun(t, repo, env, "git", "config", "--get-all", "hook.hk-pre-push.command")
	cmds := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(cmds) != 1 || cmds[0] != wantCommand {
		t.Fatalf("hook commands = %q, want exactly %q", cmds, wantCommand)
	}
}

// TestInstallJJSecondaryWorkspace writes into the clone's shared git dir from a jj secondary workspace.
func TestInstallJJSecondaryWorkspace(t *testing.T) {
	root := t.TempDir()
	env := gitEnv(root,
		"GIT_CEILING_DIRECTORIES="+root,
		"JJ_CONFIG=/dev/null",
		"JJ_USER=t",
		"JJ_EMAIL=t@t",
	)
	clone := filepath.Join(root, "clone")
	second := filepath.Join(root, "second")
	mustRun(t, root, env, "jj", "git", "init", "--colocate", clone)
	mustRun(t, clone, env, "jj", "workspace", "add", second)
	stderr, code := installer(t, second, env)
	if stderr != "" {
		t.Fatalf("install stderr = %q, want empty", stderr)
	}
	if code != 0 {
		t.Fatal("install failed in a jj secondary workspace")
	}
	stdout := mustRun(t, clone, env, "git", "config", "--get", "hook.hk-pre-push.command")
	if got := strings.TrimSuffix(stdout, "\n"); got != wantCommand {
		t.Fatalf("clone hook command = %q, want %q", got, wantCommand)
	}
}

// TestInstallOutsideRepoFailsLoud checks that install reports a directory that is no repo or workspace.
func TestInstallOutsideRepoFailsLoud(t *testing.T) {
	root := t.TempDir()
	plain := filepath.Join(root, "plain")
	if err := os.Mkdir(plain, 0o700); err != nil {
		t.Fatal(err)
	}
	stderr, code := installer(t, plain, gitEnv(root, "GIT_CEILING_DIRECTORIES="+root))
	if code == 0 {
		t.Fatal("install succeeded outside any repo or workspace")
	}
	if !strings.Contains(stderr, "not a git repo or jj workspace") {
		t.Fatalf("stderr = %q, want the not-a-repo message", stderr)
	}
}
