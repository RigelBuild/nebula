package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/rjc/internal/hookshell"
)

// fakeSSH drops ssh options, then runs the remote command with sh on this machine.
// A hook transfer frames the bundle as "<size>\n<bytes>" and keeps stdin open; the fake reads the frame, then re-sends it
// and passes the rest of stdin through, so the remote guard still sees EOF when the client ends. For the hk run pre-push
// call it copies the bundle to RJC_TEST_SSH_PAYLOAD and, once, exits 75 when RJC_TEST_SSH_STALE_MARK is set.
const fakeSSH = `#!/bin/sh
if [ -n "$RJC_TEST_SSH_ARGS" ]; then printf '%s\0' "$@" > "$RJC_TEST_SSH_ARGS"; fi
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o|-i) shift 2 ;;
    -*) shift ;;
    *) break ;;
  esac
done
if [ -n "$RJC_TEST_SSH_FAIL" ]; then exit 255; fi
shift
buf=$(mktemp)
case "$*" in
  *rjc-guard*) IFS= read -r n; head -c "$n" > "$buf" ;;
  *) n=; cat > "$buf" ;;
esac
case "$*" in
  *"hk run pre-push"*)
    if [ -n "$RJC_TEST_SSH_MISSING_CONFIG" ]; then rm -f "$buf"; exit 79; fi
    if [ -n "$RJC_TEST_SSH_PAYLOAD" ]; then cp "$buf" "$RJC_TEST_SSH_PAYLOAD"; fi
    if [ -n "$RJC_TEST_SSH_STALE_MARK" ] && [ ! -e "$RJC_TEST_SSH_STALE_MARK" ]; then touch "$RJC_TEST_SSH_STALE_MARK"; rm -f "$buf"; exit 75; fi
    if [ -n "$RJC_TEST_SSH_SLOT_MARK" ] && [ ! -e "$RJC_TEST_SSH_SLOT_MARK" ]; then touch "$RJC_TEST_SSH_SLOT_MARK"; rm -f "$buf"; exit 77; fi
    ;;
esac
unset RJC_HOOK_SSH_SLOTS
in=$buf
if [ -n "$n" ]; then
  fifo=$(mktemp -u); mkfifo "$fifo"
  # A background job's stdin is /dev/null, so dup the client stdin to fd 3 in this shell first.
  exec 3<&0
  (printf '%s\n' "$n"; cat "$buf"; exec cat <&3) > "$fifo" &
  feed=$!
  in=$fifo
fi
sh -c "$*" < "$in" 3<&-
rc=$?
if [ -n "$n" ]; then kill "$feed" 2>/dev/null; rm -f "$fifo"; fi
rm -f "$buf"
exit $rc
`

// fakeHK writes the moon and gate reports the way the gate runner does, then exits per mode.
const fakeHK = `#!/bin/sh
if [ -n "$RJC_TEST_HK_DEVENV" ]; then mkdir -p .devenv && echo warm >> .devenv/marker; fi
if [ -n "$RJC_TEST_HK_SLOTS_FILE" ]; then printf '%s' "$RJC_HOOK_SSH_SLOTS" > "$RJC_TEST_HK_SLOTS_FILE"; fi
case "$RJC_TEST_HK_MODE" in
  fail) status=failed; gates='[{"name":"gate-a","exit":1}]'; rc=1 ;;
  *) status=passed; gates='[]'; rc=0 ;;
esac
printf '{"actions":[{"status":"%s","node":{"action":"run-task","params":{"target":"a:test"}}}]}' "$status" > "$RJC_MOON_REPORT"
if [ -n "$RJC_GATE_REPORT" ]; then printf '%s' "$gates" > "$RJC_GATE_REPORT"; fi
if [ "$RJC_TEST_HK_MODE" = mutate ]; then echo changed >> README.md; fi
exit "$rc"
`

const fakeNix = `#!/bin/sh
echo "$*" >> "$RJC_TEST_NIX_LOG"
case "$*" in
  *" build "*)
    if [ -n "$RJC_TEST_NIX_BUILD_FAIL" ]; then echo "fake nix build failed" >&2; exit 1; fi
    store=${RJC_TEST_NIX_STORE:?RJC_TEST_NIX_STORE must point to a temporary test directory}
    for a in "$@"; do
      case "$a" in
        *#*) name=${a##*#}; case "$name" in ""|*/*|.|..) exit 1 ;; esac
          if [ "$name" = "$RJC_TEST_NIX_BUILD_FAIL_PACKAGE" ]; then echo "fake nix build failed for $name" >> "$RJC_TEST_NIX_LOG"; echo "fake nix build failed for $name" >&2; exit 1; fi
          dir="$store/$name"; mkdir -p "$dir/bin"
          if [ "$name" = nodejs_24 ]; then printf '#!/bin/sh\nexit 0\n' > "$dir/bin/node"; chmod +x "$dir/bin/node"
          else cp "$RJC_TEST_FAKE_HK" "$dir/bin/$name"; chmod +x "$dir/bin/$name"; fi
          echo "$dir" ;;
      esac
    done
    exit 0 ;;
esac
last=0; n=0
for a in "$@"; do n=$((n+1)); case "$a" in --|-c) last=$n ;; sh) break ;; esac; done
if [ "$last" -eq 0 ]; then exit 0; fi
shift "$last"
exec "$@"
`

// sshGateFixture returns a fixture whose ssh runner drives fake ssh and hk on this machine, with no devenv.
func sshGateFixture(t *testing.T) (*fixture, sshHookRunner) {
	t.Helper()
	f := fixtureRepo(t)
	f.repo.profile.HookReports = true
	bin := filepath.Join(os.Getenv("HOME"), "bin")
	writeFakeTool(t, filepath.Join(bin, "ssh"), fakeSSH)
	// The fixture symlinks hk to the test binary. Remove the link first so the write never reaches the test binary.
	if err := os.Remove(filepath.Join(bin, "hk")); err != nil {
		t.Fatal(err)
	}
	writeFakeTool(t, filepath.Join(bin, "hk"), fakeHK)
	writeFakeTool(t, filepath.Join(bin, "nix"), fakeNix)
	writeTestFile(t, filepath.Join(f.repo.root, "devenv.nix"), "{}\n")
	t.Setenv("RJC_TEST_NIX_LOG", filepath.Join(t.TempDir(), "nix-log"))
	t.Setenv("RJC_TEST_SSH_ARGS", filepath.Join(t.TempDir(), "ssh-args"))
	return f, sshHookRunner{repo: f.repo, host: "gate@fixture.invalid"}
}

func writeFakeTool(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}

// hostPathWithout returns a restricted PATH without the requested runner. It keeps $HOME/bin for fake tools and symlinks
// only the remote script dependencies, so the runner cannot come from the ambient PATH.
func hostPathWithout(t *testing.T, tool string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"sh", "env", "git", "mktemp", "mkfifo", "flock", "find", "touch", "timeout", "tail", "true", "seq", "rm", "mkdir", "cat", "cp", "chmod", "mv", "dirname", "tar", "gzip", "sed", "paste", "tr", "df", "nproc", "uname", "jj", "setsid", "ps", "awk", "head", "wc", "kill", "sleep"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("host PATH needs %s, which is not on the ambient PATH: %v", name, err)
		}
		if err := os.Symlink(path, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(os.Getenv("HOME"), "bin") + string(os.PathListSeparator) + dir
	t.Setenv("PATH", path)
	if found, err := exec.LookPath(tool); err == nil {
		t.Fatalf("%s must be absent on the test PATH, but resolves to %s", tool, found)
	}
	return path
}

func sshRange(t *testing.T, f *fixture) (string, string) {
	t.Helper()
	base := jjTest(t, f.repo, "log", "--no-graph", "-r", mainOrigin, "-T", "commit_id")
	head := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	return base, head
}

func TestSSHHookRunnerPasses(t *testing.T) {
	f, runner := sshGateFixture(t)
	f.commit(t, "hk.pkl")
	base, head := sshRange(t, f)
	out, _, err := runner.Run(f.ctx, base, head, nil, io.Discard)
	if err != nil || out.Phase != phaseCompleted || out.Exit != 0 || out.From != base || out.To != head || len(out.FailedTargets) != 0 || len(out.FailedGates) != 0 {
		t.Fatalf("pass: %+v err=%v", out, err)
	}
}
func TestSSHHookRunnerConfiguredTools(t *testing.T) {
	for _, tc := range []struct{ name, config, tool string }{
		{name: "lefthook", config: "lefthook.yml", tool: "lefthook"},
		{name: "pre-commit", config: ".pre-commit-config.yaml", tool: "pre-commit"},
		{name: "pre-commit-yml", config: ".pre-commit-config.yml", tool: "pre-commit"},
		{name: "prek", config: "prek.toml", tool: "prek"},
		{name: "lefthook-config", config: ".config/lefthook.toml", tool: "lefthook"},
		{name: "prek-dot", config: ".prek.toml", tool: "prek"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, runner := sshGateFixture(t)
			marker := filepath.Join(t.TempDir(), "tool")
			t.Setenv("RJC_TEST_TOOL_MARKER", marker)
			writeFakeTool(t, filepath.Join(os.Getenv("HOME"), "bin", tc.tool), "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$RJC_TEST_TOOL_MARKER\"\n"+`if [ "$1" = run ] && [ "$2" = pre-push ]; then cat > "$RJC_TEST_TOOL_MARKER.stdin"; fi`+"\n"+fakeHK)
			f.commit(t, tc.config)
			base, head := sshRange(t, f)
			out, _, err := runner.Run(f.ctx, base, head, nil, io.Discard)
			if err != nil || out.Phase != phaseCompleted || out.Exit != 0 {
				t.Fatalf("configured tool: %+v err=%v", out, err)
			}
			got, err := os.ReadFile(marker)
			if err != nil {
				t.Fatalf("%s did not run: %v", tc.tool, err)
			}
			want := "run --hook-stage pre-push --from-ref " + base + " --to-ref " + head + "\n"
			if tc.tool == "pre-commit" || tc.tool == "prek" {
				want = "run --config " + tc.config + " --hook-stage pre-push --from-ref " + base + " --to-ref " + head + "\n"
			}
			if tc.tool == "lefthook" {
				want = "run pre-push origin rjc-ssh\n"
				stdinFile := marker + ".stdin"
				stdin, err := os.ReadFile(stdinFile)
				if err != nil || string(stdin) != "refs/heads/x "+head+" refs/heads/x "+base+"\n" {
					t.Fatalf("lefthook stdin = %q, err=%v", stdin, err)
				}
			}
			if string(got) != want {
				t.Fatalf("%s args = %q, want %q", tc.tool, got, want)
			}
		})
	}
}

func TestSSHHookRunnerReportsFailures(t *testing.T) {
	f, runner := sshGateFixture(t)
	f.commit(t, "hk.pkl")
	base, head := sshRange(t, f)
	t.Setenv("RJC_TEST_HK_MODE", "fail")
	out, _, err := runner.Run(f.ctx, base, head, nil, io.Discard)
	if err != nil || out.Phase != phaseCompleted || out.Exit != 1 || !slices.Equal(out.FailedTargets, []string{"a:test"}) || !slices.Equal(out.FailedGates, []string{"gate-a"}) {
		t.Fatalf("fail: %+v err=%v", out, err)
	}
}

func TestSSHHookRunnerNoConfig(t *testing.T) {
	f, runner := sshGateFixture(t)
	f.commit(t, "feature.txt")
	base, head := sshRange(t, f)
	out, _, err := runner.Run(f.ctx, base, head, nil, io.Discard)
	if err != nil || out.Phase != phaseNoConfig || out.Exit != 0 {
		t.Fatalf("no-config: %+v err=%v", out, err)
	}
	if _, err := os.Stat(os.Getenv("RJC_TEST_SSH_ARGS")); !os.IsNotExist(err) {
		t.Fatalf("ssh was invoked: %v", err)
	}
}
func TestSSHHookRunnerMissingRemoteConfig(t *testing.T) {
	f, runner := sshGateFixture(t)
	f.commit(t, "lefthook.yml")
	base, head := sshRange(t, f)
	t.Setenv("RJC_TEST_SSH_MISSING_CONFIG", "1")
	out, _, err := runner.Run(f.ctx, base, head, nil, io.Discard)
	if out.Phase != phaseSetupFailed || err == nil || !strings.Contains(err.Error(), "remote tree lacks lefthook.yml") {
		t.Fatalf("missing config: %+v err=%v", out, err)
	}
}

func TestSSHHookRunnerMissingTool(t *testing.T) {
	f, runner := sshGateFixture(t)
	if err := os.Remove(filepath.Join(os.Getenv("HOME"), "bin", "hk")); err != nil {
		t.Fatal(err)
	}
	path := hostPathWithout(t, "lefthook")
	f.repo.env = append(f.repo.env, "PATH="+path)
	t.Setenv("RJC_HOOK_SSH_SHELL", "env")
	t.Setenv("RJC_TEST_NIX_BUILD_FAIL", "1")
	t.Setenv("RJC_TEST_NIX_STORE", t.TempDir())
	f.commit(t, "lefthook.yml")
	base, head := sshRange(t, f)
	out, _, err := runner.Run(f.ctx, base, head, nil, io.Discard)
	if out.Phase != phaseSetupFailed || err == nil || !strings.Contains(err.Error(), "remote shell lacks lefthook") {
		t.Fatalf("missing runner: %+v err=%v", out, err)
	}
	log, err := os.ReadFile(os.Getenv("RJC_TEST_NIX_LOG"))
	if err != nil || !strings.Contains(string(log), "build --no-link --print-out-paths "+hookshell.Nixpkgs+"#lefthook") {
		t.Fatalf("missing runner must attempt nixpkgs fallback, got nix log %q err=%v", log, err)
	}
}
func TestSSHHookBodyRejectsMissingConfig(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFakeTool(t, filepath.Join(bin, "git"), "#!/bin/sh\nexit 0\n")
	cmd := exec.CommandContext(t.Context(), "sh", "-c", sshHookBody, "sh", "base", "head", "0", home, home, home, "unused", "lefthook", "lefthook.yml", "30s")
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	exit, ok := errors.AsType[*exec.ExitError](err)
	if !ok || exit.ExitCode() != sshMissingConfig || !strings.Contains(string(out), "remote tree lacks lefthook.yml") {
		t.Fatalf("missing config: output=%s err=%v", out, err)
	}
}

func TestSSHHookRunnerMutatedTreeFails(t *testing.T) {
	f, runner := sshGateFixture(t)
	f.commit(t, "hk.pkl")
	base, head := sshRange(t, f)
	t.Setenv("RJC_TEST_HK_MODE", "mutate")
	var log bytes.Buffer
	out, _, err := runner.Run(f.ctx, base, head, nil, &log)
	if err != nil || out.Phase != phaseCompleted || out.Exit != 1 || !strings.Contains(log.String(), "hook-mutated-tree") {
		t.Fatalf("mutated: %+v err=%v log=%s", out, err, &log)
	}
}

func TestSSHHookRunnerTransportFailureRecordsNothing(t *testing.T) {
	f, runner := sshGateFixture(t)
	f.commit(t, "hk.pkl")
	head := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	line := []Change{{Bookmark: "test-lane/feature", Tip: head, Base: mainOrigin, Owned: true}}
	store := localPrePush{root: f.repo.stateDir}
	t.Setenv("RJC_TEST_SSH_FAIL", "1")
	err := f.repo.runHooks(f.ctx, line, nil, runner, store)
	detail, ok := errors.AsType[*failure](err)
	if !ok || detail.stage != "hook-ssh" || detail.code != 3 {
		t.Fatalf("transport failure: %v", err)
	}
	if _, found, err := store.Read(f.ctx, f.repo.profile.Name, 0, head); err != nil || found {
		t.Fatalf("transport failure recorded evidence: found=%v err=%v", found, err)
	}
}

func TestSSHHookRunnerRefusesSkips(t *testing.T) {
	f, runner := sshGateFixture(t)
	base, head := sshRange(t, f)
	_, _, err := runner.Run(f.ctx, base, head, []SkippedTarget{{Target: "a:test", Reason: "EX-1"}}, io.Discard)
	assertRefusalGate(t, err, "skip-target-unsupported-over-ssh")
}

func TestSSHHookRunnerRejectsOptionLikeHost(t *testing.T) {
	f, runner := sshGateFixture(t)
	runner.host = "-oProxyCommand=sh"
	base, head := sshRange(t, f)
	_, _, err := runner.Run(f.ctx, base, head, nil, io.Discard)
	assertRefusalGate(t, err, "hook-ssh-config")
}

func TestSSHHookRunnerQuotesIdentity(t *testing.T) {
	f, runner := sshGateFixture(t)
	f.commit(t, "hk.pkl")
	base, head := sshRange(t, f)
	runner.identity = filepath.Join(t.TempDir(), `key "one".pem`)
	if _, _, err := runner.Run(f.ctx, base, head, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(os.Getenv("RJC_TEST_SSH_ARGS"))
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
	index := slices.Index(args, "-i")
	if index < 0 || index+1 >= len(args) || args[index+1] != runner.identity {
		t.Fatalf("identity argv: %q", args)
	}
}

// TestSSHHookRunnerPassesSlotCountToRemote proves the slot count reaches the remote hook body.
// ssh does not forward the client env, so the count must travel as an argument and be exported remotely.
func TestSSHHookRunnerPassesSlotCountToRemote(t *testing.T) {
	f, runner := sshGateFixture(t)
	t.Setenv("RJC_HOOK_SSH_SLOTS", "1")
	t.Setenv("RJC_TEST_HK_SLOTS_FILE", filepath.Join(t.TempDir(), "hook-slots"))
	f.commit(t, "hk.pkl")
	base, head := sshRange(t, f)
	if _, _, err := runner.Run(f.ctx, base, head, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(os.Getenv("RJC_TEST_SSH_ARGS"))
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
	remote := args[len(args)-1]
	if !strings.Contains(remote, " 1 'hk' 'hk.pkl' '") {
		t.Fatalf("remote command must pass slot count 1 before runner and config, got %q", remote)
	}
	if !strings.Contains(remote, f.repo.profile.DevenvNixpkgs+"#devenv") {
		t.Fatalf("remote script must start devenv from the profile's devenvNixpkgs %q, got %q", f.repo.profile.DevenvNixpkgs, remote)
	}
	slots, err := os.ReadFile(os.Getenv("RJC_TEST_HK_SLOTS_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	if string(slots) != "1" {
		t.Fatalf("remote hook RJC_HOOK_SSH_SLOTS = %q, want 1", slots)
	}
}

func TestSSHHookRunnerPassesTimeoutToRemote(t *testing.T) {
	f, runner := sshGateFixture(t)
	t.Setenv("RJC_HOOK_SSH_TIMEOUT", "37s")
	f.commit(t, "hk.pkl")
	base, head := sshRange(t, f)
	if _, _, err := runner.Run(f.ctx, base, head, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(os.Getenv("RJC_TEST_SSH_ARGS"))
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
	fields := strings.Fields(args[len(args)-1])
	limit, err := time.ParseDuration(strings.Trim(fields[len(fields)-1], "'"))
	if err != nil || limit <= 0 || limit > 37*time.Second || limit < 30*time.Second {
		t.Fatalf("remote timeout = %s, error %v", limit, err)
	}
}

// TestSSHHookRunnerBoundsRemoteSetupAndKeepsAlive proves the whole remote command runs under the guard with a limit
// just under the client deadline, so setup is bounded too, and that ssh sends keepalives so a dead link ends the client.
func TestSSHHookRunnerBoundsRemoteSetupAndKeepsAlive(t *testing.T) {
	f, runner := sshGateFixture(t)
	t.Setenv("RJC_HOOK_SSH_TIMEOUT", "37s")
	f.commit(t, "hk.pkl")
	base, head := sshRange(t, f)
	if _, _, err := runner.Run(f.ctx, base, head, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(os.Getenv("RJC_TEST_SSH_ARGS"))
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
	for _, want := range []string{"ServerAliveInterval=15", "ServerAliveCountMax=4"} {
		if !slices.Contains(args, want) {
			t.Fatalf("ssh args must include %s, got %q", want, args)
		}
	}
	remote := args[len(args)-1]
	const marker = " rjc-guard '"
	if !strings.Contains(remote, " sh -c "+shQuote(sshHookGuard)+marker) {
		t.Fatalf("remote command must run under the guard, got %q", remote)
	}
	rest := remote[strings.Index(remote, marker)+len(marker):]
	end := strings.IndexByte(rest, '\'')
	if end < 0 {
		t.Fatalf("remote deadline is not quoted: %q", remote)
	}
	limit, err := time.ParseDuration(rest[:end])
	if err != nil || limit < 25*time.Second || limit > 37*time.Second-sshDeadlineMargin {
		t.Fatalf("remote deadline = %s, error %v; want under the 37s client deadline by the margin", limit, err)
	}
}

func TestSSHHookRunnerRemoteTimeoutIsToolFailure(t *testing.T) {
	f, runner := sshGateFixture(t)
	t.Setenv("RJC_HOOK_SSH_TIMEOUT", "30s")
	bin := filepath.Join(os.Getenv("HOME"), "bin")
	writeFakeTool(t, filepath.Join(bin, "timeout"), "#!/bin/sh\nexit 124\n")
	f.commit(t, "hk.pkl")
	base, head := sshRange(t, f)
	out, _, err := runner.Run(f.ctx, base, head, nil, io.Discard)
	fail, ok := errors.AsType[*failure](err)
	if !ok || fail.stage != "hook-ssh-timeout" || out.Phase == phaseCompleted {
		t.Fatalf("remote timeout outcome = %+v, error = %v", out, err)
	}
}

func TestSSHHookRunnerHookExit124(t *testing.T) {
	f, runner := sshGateFixture(t)
	f.repo.profile.HookReports = false
	writeFakeTool(t, filepath.Join(os.Getenv("HOME"), "bin", "hk"), "#!/bin/sh\nexit 124\n")
	f.commit(t, "hk.pkl")
	base, head := sshRange(t, f)
	out, _, err := runner.Run(f.ctx, base, head, nil, io.Discard)
	if err != nil || out.Phase != phaseCompleted || out.Exit != 124 {
		t.Fatalf("hook exit 124: outcome %+v, error %v", out, err)
	}
}

func TestSSHHookRunnerRealRemoteTimeout(t *testing.T) {
	f, runner := sshGateFixture(t)
	f.repo.profile.HookReports = false
	writeFakeTool(t, filepath.Join(os.Getenv("HOME"), "bin", "hk"), "#!/bin/sh\nsleep 5\n")
	f.commit(t, "hk.pkl")
	base, head := sshRange(t, f)
	// Keep the client deadline independent from the host's one-second limit.
	ctx := context.WithoutCancel(f.ctx)
	payload := filepath.Join(t.TempDir(), "payload")
	ref := "refs/rjc-hook/test-timeout"
	if _, err := f.repo.command(ctx, nil, nil, "git", "update-ref", ref, head); err != nil {
		t.Fatal(err)
	}
	if err := runner.makeBundle(ctx, payload, ref, nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RJC_HOOK_SSH_TIMEOUT", "1s")
	_, err := runner.transfer(ctx, payload, base, head, "hk", "hk.pkl", io.Discard)
	fail, ok := errors.AsType[*failure](err)
	if !ok || fail.stage != "hook-ssh-timeout" {
		t.Fatalf("real remote timeout: %v", err)
	}
}

// hangSetup makes remote setup hang in bundle unbundle, under the slot lock. The fake git starts a grandchild in its
// own process group (timeout calls setpgid), the way an inner timeout or nix build does, and returns its PID file.
func hangSetup(t *testing.T) (hung, grandchild string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	hung, grandchild = filepath.Join(dir, "hung"), filepath.Join(dir, "grandchild")
	t.Setenv("RJC_TEST_HUNG", hung)
	t.Setenv("RJC_TEST_GRANDCHILD", grandchild)
	writeFakeTool(t, filepath.Join(os.Getenv("HOME"), "bin", "git"), "#!/bin/sh\ncase \"$*\" in *'bundle unbundle'*)\n  timeout 600 sleep 600 & printf '%s' \"$!\" > \"$RJC_TEST_GRANDCHILD\"\n  : > \"$RJC_TEST_HUNG\"; wait; exit 1 ;;\nesac\nexec "+shQuote(real)+" \"$@\"\n")
	return hung, grandchild
}

// eventually polls cond until it holds or bound passes. The remote processes signal nothing back, so this is a bounded poll.
func eventually(t *testing.T, bound time.Duration, what string, cond func() bool) {
	t.Helper()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	limit := time.After(bound)
	for !cond() {
		select {
		case <-tick.C:
		case <-limit:
			t.Fatalf("%s: not within %s", what, bound)
		}
	}
}

// remoteReleased reports whether the gate slot is free and the grandchild has exited.
func remoteReleased(t *testing.T, grandchild string) bool {
	t.Helper()
	slot := filepath.Join(os.Getenv("HOME"), "rjc-hook", "slots", "s1")
	if exec.Command("flock", "-n", slot, "true").Run() != nil {
		return false
	}
	data, err := os.ReadFile(grandchild)
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(data))
	if exec.Command("kill", "-0", pid).Run() != nil {
		return true
	}
	stat, err := exec.Command("ps", "-o", "stat=", "-p", pid).Output()
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(stat)), "Z")
}

// stopGrandchild kills a leftover grandchild when the test failed, so a red run leaves nothing behind.
func stopGrandchild(t *testing.T, grandchild string) {
	t.Helper()
	t.Cleanup(func() {
		data, err := os.ReadFile(grandchild)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			t.Errorf("grandchild pid: %v", err)
			return
		}
		if process, err := os.FindProcess(pid); err == nil {
			if err := process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Logf("grandchild %d: %v", pid, err)
			}
		}
	})
}

// TestSSHHookRunnerCancelReleasesSlot cancels the client while remote setup hangs under the slot lock. ssh dies and its
// stdin closes; the remote must then end its whole session, so the slot frees and the separate-group grandchild exits.
func TestSSHHookRunnerCancelReleasesSlot(t *testing.T) {
	f, runner := sshGateFixture(t)
	f.repo.profile.HookReports = false
	t.Setenv("RJC_HOOK_SSH_TIMEOUT", "10m")
	hung, grandchild := hangSetup(t)
	stopGrandchild(t, grandchild)
	f.commit(t, "hk.pkl")
	base, head := sshRange(t, f)
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := runner.Run(ctx, base, head, nil, io.Discard)
		done <- err
	}()
	eventually(t, 30*time.Second, "remote setup to hang", func() bool { _, err := os.Stat(hung); return err == nil })
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled run reported success")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("cancelled run did not return within 20s")
	}
	eventually(t, 20*time.Second, "slot and grandchild released after cancel", func() bool { return remoteReleased(t, grandchild) })
}

// TestSSHHookRunnerGroupKillReleasesSlot mimics Tailscale SSH, which SIGKILLs the session's process group on
// disconnect. The guard dies with that group, so only a watchdog in its own session can still reap the run.
func TestSSHHookRunnerGroupKillReleasesSlot(t *testing.T) {
	f, runner := sshGateFixture(t)
	f.repo.profile.HookReports = false
	t.Setenv("RJC_HOOK_SSH_TIMEOUT", "10m")
	hung, grandchild := hangSetup(t)
	stopGrandchild(t, grandchild)
	leader := filepath.Join(t.TempDir(), "leader")
	t.Setenv("RJC_TEST_SSH_LEADER", leader)
	// The remote shell leads its own session and process group, as under an ssh server, and records its PID.
	writeFakeTool(t, filepath.Join(os.Getenv("HOME"), "bin", "ssh"), `#!/bin/sh
while [ "$1" = -o ] || [ "$1" = -i ]; do shift 2; done
shift 2
exec setsid sh -c 'printf "%s" "$$" > "$RJC_TEST_SSH_LEADER"; exec sh -c "$1"' rjc-sshd "$*"
`)
	f.commit(t, "hk.pkl")
	base, head := sshRange(t, f)
	runner.probe = func(context.Context, string) (hostStatus, error) {
		return hostStatus{nproc: 1, freeGiB: 100, freeSlots: 1}, nil
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := runner.Run(f.ctx, base, head, nil, io.Discard)
		done <- err
	}()
	eventually(t, 30*time.Second, "remote setup to hang", func() bool { _, err := os.Stat(hung); return err == nil })
	data, err := os.ReadFile(leader)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("kill", "-KILL", "--", "-"+strings.TrimSpace(string(data))).CombinedOutput(); err != nil {
		t.Fatalf("kill the ssh process group: %v %s", err, out)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("run reported success after its ssh session was killed")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("run did not return within 20s after its ssh session was killed")
	}
	eventually(t, 20*time.Second, "slot and grandchild released after the group kill", func() bool { return remoteReleased(t, grandchild) })
}

// TestSSHHookRunnerDeadlineReleasesSlot lets the remote deadline expire while setup hangs. The run fails as
// hook-ssh-timeout, and the remote session, the separate-group grandchild included, is gone with the slot free.
func TestSSHHookRunnerDeadlineReleasesSlot(t *testing.T) {
	f, runner := sshGateFixture(t)
	f.repo.profile.HookReports = false
	t.Setenv("RJC_HOOK_SSH_TIMEOUT", "9s")
	_, grandchild := hangSetup(t)
	stopGrandchild(t, grandchild)
	f.commit(t, "hk.pkl")
	base, head := sshRange(t, f)
	done := make(chan error, 1)
	go func() {
		_, _, err := runner.Run(f.ctx, base, head, nil, io.Discard)
		done <- err
	}()
	var err error
	select {
	case err = <-done:
	case <-time.After(40 * time.Second):
		t.Fatal("deadline run did not return within 40s")
	}
	fail, ok := errors.AsType[*failure](err)
	if !ok || fail.stage != "hook-ssh-timeout" {
		t.Fatalf("deadline run: %v", err)
	}
	eventually(t, 20*time.Second, "slot and grandchild released after the deadline", func() bool { return remoteReleased(t, grandchild) })
}

func TestSSHHookWorktreeLockRetries(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "pool")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFakeTool(t, filepath.Join(bin, "git"), "#!/bin/sh\nexit 0\n")
	flock, err := exec.LookPath("flock")
	if err != nil {
		t.Fatal(err)
	}
	writeFakeTool(t, filepath.Join(bin, "flock"), "#!/bin/sh\ncase \"$*\" in *' -E 76 '*w0.lock*) exit 76 ;; esac\nexec "+shQuote(flock)+" \"$@\"\n")
	d := filepath.Join(home, "run")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "sh", "-c", sshHookSlot, "sh", d, root, "cache", "base", "head", "0", "printf '%s' \"$4\"", "hook", "hk", "hk.pkl", "30s")
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil || string(out) != filepath.Join(root, "w1") {
		t.Fatalf("worktree contention: output %q, error %v", out, err)
	}
}

func TestSSHHookConcurrentWorktrees(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "pool")
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFakeTool(t, filepath.Join(bin, "git"), "#!/bin/sh\nexit 0\n")
	body := `printf '%s\n' "$4"; sleep 2`
	var commands [2]*exec.Cmd
	var output [2]bytes.Buffer
	for i := range commands {
		d := filepath.Join(home, strconv.Itoa(i))
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		commands[i] = exec.CommandContext(t.Context(), "sh", "-c", sshHookSlot, "sh", d, root, "cache", "base", "head", "0", body, "hook", "hk", "hk.pkl", "30s")
		commands[i].Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		commands[i].Stdout = &output[i]
		commands[i].Stderr = &output[i]
		if err := commands[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	for i, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("run %d: %v: %s", i, err, output[i].String())
		}
	}
	if output[0].String() == output[1].String() {
		t.Fatalf("concurrent hooks used the same worktree: %q", output[0].String())
	}
}

// TestSSHHookRunnerRefusesBadSlotCount proves a non-integer or zero RJC_HOOK_SSH_SLOTS is refused as a config error,
// before any ssh runs, so a typo is not reported as an unreachable host.
func TestSSHHookRunnerRefusesBadSlotCount(t *testing.T) {
	for _, bad := range []string{"two", "0", "-1"} {
		t.Run(bad, func(t *testing.T) {
			f, runner := sshGateFixture(t)
			t.Setenv("RJC_HOOK_SSH_SLOTS", bad)
			f.commit(t, "hk.pkl")
			base, head := sshRange(t, f)
			_, _, err := runner.Run(f.ctx, base, head, nil, io.Discard)
			fail, ok := errors.AsType[*failure](err)
			if !ok || fail.gate != "hook-ssh-config" {
				t.Fatalf("RJC_HOOK_SSH_SLOTS=%q error = %v, want hook-ssh-config", bad, err)
			}
		})
	}
}

func TestShellQuoteRoundTrips(t *testing.T) {
	for _, value := range []string{"plain", "with space", `quote'and"more`, "$HOME `id`"} {
		out, err := exec.Command("sh", "-c", "printf %s "+shQuote(value)).Output()
		if err != nil || string(out) != value {
			t.Fatalf("%q round-tripped to %q: %v", value, out, err)
		}
	}
}

func TestHookRunnerSelectsSSHFromEnvironment(t *testing.T) {
	f := fixtureRepo(t)
	t.Setenv("RJC_HOOK_LOCAL", "refuse")
	t.Setenv("RJC_HOOK_SSH", "")
	if _, ok := f.repo.hookRunner().(localHookRunner); !ok {
		t.Fatal("RJC_HOOK_LOCAL alone must keep the local runner selected")
	}
	t.Setenv("RJC_HOOK_SSH", "gate@fixture.invalid")
	runner, ok := f.repo.hookRunner().(sshHookRunner)
	if !ok || len(runner.hosts) != 1 || runner.hosts[0] != "gate@fixture.invalid" {
		t.Fatalf("RJC_HOOK_LOCAL=refuse must still select RJC_HOOK_SSH: %+v", f.repo.hookRunner())
	}
	t.Setenv("RJC_HOOK_SSH", "a@one.invalid, b@two.invalid,")
	runner, ok = f.repo.hookRunner().(sshHookRunner)
	if !ok || !slices.Equal(runner.hosts, []string{"a@one.invalid", "b@two.invalid"}) {
		t.Fatalf("comma-list RJC_HOOK_SSH hosts = %v, want both hosts with the empty item dropped", runner.hosts)
	}
	t.Setenv("RJC_HOOK_LOCAL", "")
	t.Setenv("RJC_HOOK_SSH", "")
	if _, ok := f.repo.hookRunner().(localHookRunner); !ok {
		t.Fatal("unset hook environment must keep the local runner")
	}
}

// fakeProbe returns the status or error registered for each host, so tests never run ssh.
func fakeProbe(t *testing.T, statuses map[string]hostStatus, failing map[string]bool) hostProbe {
	t.Helper()
	return func(_ context.Context, host string) (hostStatus, error) {
		if failing[host] {
			return hostStatus{}, errors.New("connection refused")
		}
		status, ok := statuses[host]
		if !ok {
			t.Fatalf("probe asked for unknown host %q", host)
		}
		return status, nil
	}
}

func TestPickHostChoosesLowestLoad(t *testing.T) {
	hosts := []string{"busy@a", "idle@b"}
	probe := fakeProbe(t, map[string]hostStatus{
		"busy@a": {nproc: 4, load1: 3.2, freeGiB: 100, freeSlots: 1},
		"idle@b": {nproc: 4, load1: 0.4, freeGiB: 100, freeSlots: 1},
	}, nil)
	pick, err := pickHost(t.Context(), hosts, probe)
	if err != nil {
		t.Fatal(err)
	}
	if pick.chosen != "idle@b" {
		t.Fatalf("chosen = %q, want the lower load idle@b", pick.chosen)
	}
}

func TestPickHostSkipsFullDisk(t *testing.T) {
	hosts := []string{"idle@a", "busy@b"}
	probe := fakeProbe(t, map[string]hostStatus{
		"idle@a": {nproc: 4, load1: 0.1, freeGiB: 2, freeSlots: 1},
		"busy@b": {nproc: 4, load1: 2.0, freeGiB: 60, freeSlots: 1},
	}, nil)
	pick, err := pickHost(t.Context(), hosts, probe)
	if err != nil {
		t.Fatal(err)
	}
	if pick.chosen != "busy@b" {
		t.Fatalf("chosen = %q, want busy@b: idle@a has too little free space", pick.chosen)
	}
}

// TestHostProbeMeasuresPoolDisk checks the pool disk separately from /tmp.
func TestHostProbeMeasuresPoolDisk(t *testing.T) {
	home := t.TempDir()
	tmpDF, err := exec.Command("df", "-Pk", "/tmp").Output()
	if err != nil {
		t.Fatal(err)
	}
	homeDF, err := exec.Command("df", "-Pk", home).Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Fields(strings.Split(strings.TrimSpace(string(tmpDF)), "\n")[1])[0] == strings.Fields(strings.Split(strings.TrimSpace(string(homeDF)), "\n")[1])[0] {
		t.Skip("HOME and /tmp share a filesystem; the test cannot distinguish their free space")
	}
	cmd := exec.Command("sh", "-c", sshHostProbeScript, "rjc-probe", "2")
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("probe script: %v", err)
	}
	status, err := parseHostStatus(string(out))
	if err != nil {
		t.Fatalf("probe output %q: %v", out, err)
	}
	want, err := exec.Command("df", "-Pk", home).Output()
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(strings.Split(strings.TrimSpace(string(want)), "\n")[1])
	wantKiB, err := strconv.ParseFloat(fields[3], 64)
	if err != nil {
		t.Fatal(err)
	}
	// df reads a live pool, so allow 1% drift between measurements.
	if got := status.freeGiB * (1 << 20); math.Abs(got-wantKiB) > wantKiB*0.01 {
		t.Fatalf("probe free = %v KiB, want the free space of HOME within 1%% of %v KiB", got, wantKiB)
	}
}

func TestPickHostSkipsUnreachable(t *testing.T) {
	hosts := []string{"down@a", "up@b"}
	probe := fakeProbe(t, map[string]hostStatus{"up@b": {nproc: 2, load1: 1.0, freeGiB: 40, freeSlots: 1}}, map[string]bool{"down@a": true})
	pick, err := pickHost(t.Context(), hosts, probe)
	if err != nil {
		t.Fatal(err)
	}
	if pick.chosen != "up@b" {
		t.Fatalf("chosen = %q, want up@b: down@a is unreachable", pick.chosen)
	}
}

func TestPickHostTiesKeepListOrder(t *testing.T) {
	hosts := []string{"first@a", "second@b"}
	probe := fakeProbe(t, map[string]hostStatus{
		"first@a":  {nproc: 4, load1: 1.0, freeGiB: 50, freeSlots: 1},
		"second@b": {nproc: 4, load1: 1.0, freeGiB: 50, freeSlots: 1},
	}, nil)
	pick, err := pickHost(t.Context(), hosts, probe)
	if err != nil {
		t.Fatal(err)
	}
	if pick.chosen != "first@a" {
		t.Fatalf("chosen = %q, want first@a on a tie", pick.chosen)
	}
}

func TestPickHostAllBadIsToolFailure(t *testing.T) {
	t.Parallel()
	hosts := []string{"down@a", "full@b"}
	probe := fakeProbe(t, map[string]hostStatus{"full@b": {nproc: 4, load1: 0.1, freeGiB: 1}}, map[string]bool{"down@a": true})
	_, err := pickHost(t.Context(), hosts, probe)
	if err == nil {
		t.Fatal("no qualifying host must fail")
	}
	runner := sshHookRunner{hosts: hosts, probe: probe, repo: parallelFixtureRepo(t).repo}
	_, _, err = runner.pickRunnerHost(t.Context(), io.Discard)
	fail, ok := errors.AsType[*failure](err)
	if !ok || fail.stage != "hook-ssh-no-host" || fail.code != 3 {
		t.Fatalf("all-bad error = %v, want a tool failure with stage hook-ssh-no-host and exit 3", err)
	}
}

// fakeClock advances only when the queue waits, so the deadline passes without real time and without sleeping.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }
func (c *fakeClock) Wait(_ context.Context, d time.Duration) error {
	c.now = c.now.Add(d)
	return nil
}

func TestPickHostPrefersHostWithFreeSlot(t *testing.T) {
	hosts := []string{"idle-but-full@a", "free@b"}
	probe := fakeProbe(t, map[string]hostStatus{
		"idle-but-full@a": {nproc: 8, load1: 0.1, freeGiB: 100, freeSlots: 0},
		"free@b":          {nproc: 8, load1: 2.0, freeGiB: 100, freeSlots: 1},
	}, nil)
	pick, err := pickHost(t.Context(), hosts, probe)
	if err != nil {
		t.Fatal(err)
	}
	if pick.chosen != "free@b" {
		t.Fatalf("chosen = %q, want free@b: it has a free slot, even though idle-but-full@a has lower load", pick.chosen)
	}
}

func TestPickRunnerHostQueuesThenTimesOut(t *testing.T) {
	hosts := []string{"full@a", "full@b"}
	probe := fakeProbe(t, map[string]hostStatus{
		"full@a": {nproc: 4, load1: 0.5, freeGiB: 100, freeSlots: 0},
		"full@b": {nproc: 4, load1: 0.5, freeGiB: 100, freeSlots: 0},
	}, nil)
	clock := &fakeClock{now: time.Unix(0, 0)}
	t.Setenv("RJC_HOOK_SSH_TIMEOUT", "90s")
	var log bytes.Buffer
	runner := sshHookRunner{hosts: hosts, probe: probe, wait: clock.Wait, now: clock.Now, repo: fixtureRepo(t).repo}
	_, _, err := runner.pickRunnerHost(t.Context(), &log)
	fail, ok := errors.AsType[*failure](err)
	if !ok || fail.stage != "hook-ssh-timeout" {
		t.Fatalf("all slots busy until the deadline must be hook-ssh-timeout, got %v", err)
	}
	if got := strings.Count(log.String(), "hook-ssh: waiting for a gate slot"); got < 2 {
		t.Fatalf("the queue must log the wait each round, logged %d times: %q", got, log.String())
	}
	if !strings.Contains(err.Error(), "full@a") || !strings.Contains(err.Error(), "full@b") {
		t.Fatalf("timeout must name each host's reason, got %v", err)
	}
}

func TestPickRunnerHostWaitsThenPicksWhenSlotFrees(t *testing.T) {
	hosts := []string{"busy@a"}
	clock := &fakeClock{now: time.Unix(0, 0)}
	rounds := 0
	probe := func(_ context.Context, host string) (hostStatus, error) {
		rounds++
		free := 0
		if rounds >= 3 {
			free = 1
		}
		return hostStatus{nproc: 4, load1: 0.2, freeGiB: 100, freeSlots: free}, nil
	}
	t.Setenv("RJC_HOOK_SSH_TIMEOUT", "10m")
	var log bytes.Buffer
	runner := sshHookRunner{hosts: hosts, probe: probe, wait: clock.Wait, now: clock.Now, repo: fixtureRepo(t).repo}
	picked, pick, err := runner.pickRunnerHost(t.Context(), &log)
	if err != nil {
		t.Fatalf("a slot that frees must be picked, got %v", err)
	}
	if picked.host != "busy@a" || pick.chosen != "busy@a" {
		t.Fatalf("picked host = %q, want busy@a", picked.host)
	}
	if rounds < 3 {
		t.Fatalf("the queue must re-probe until the slot frees, probed %d times", rounds)
	}
}

// TestRunRetriesAfterLostSlotRace drives the real Run. The first remote attempt exits 77, so the slot was taken
// after the probe. Run must re-pick through the queue and run again, and the second attempt must complete.
func TestRunRetriesAfterLostSlotRace(t *testing.T) {
	f, runner := sshGateFixture(t)
	t.Setenv("RJC_TEST_SSH_SLOT_MARK", filepath.Join(t.TempDir(), "slot-marker"))
	t.Setenv("RJC_HOOK_SSH_TIMEOUT", "10m")
	clock := &fakeClock{now: time.Unix(0, 0)}
	runner.hosts = []string{"gate@fixture.invalid"}
	runner.probe = func(_ context.Context, host string) (hostStatus, error) {
		return hostStatus{nproc: 4, load1: 0.2, freeGiB: 100, freeSlots: 1}, nil
	}
	runner.wait = clock.Wait
	runner.now = clock.Now
	f.commit(t, "hk.pkl")
	base, head := sshRange(t, f)
	out, _, err := runner.Run(f.ctx, base, head, nil, io.Discard)
	if err != nil {
		t.Fatalf("a lost slot race must retry and then succeed, got %v", err)
	}
	if out.Phase != phaseCompleted {
		t.Fatalf("the retried run must complete, got phase %v", out.Phase)
	}
	if _, statErr := os.Stat(os.Getenv("RJC_TEST_SSH_SLOT_MARK")); statErr != nil {
		t.Fatal("the first remote attempt never lost the slot race, so the retry was not exercised")
	}
}

func TestPickHostSkipsOverloadedHost(t *testing.T) {
	hosts := []string{"hot@a", "idle@b"}
	probe := fakeProbe(t, map[string]hostStatus{
		"hot@a":  {nproc: 4, load1: 9.0, freeGiB: 100, freeSlots: 1},
		"idle@b": {nproc: 4, load1: 0.4, freeGiB: 100, freeSlots: 1},
	}, nil)
	pick, err := pickHost(t.Context(), hosts, probe)
	if err != nil {
		t.Fatal(err)
	}
	if pick.chosen != "idle@b" {
		t.Fatalf("chosen = %q, want idle@b: it has lower load per CPU than hot@a", pick.chosen)
	}
	if !slices.Contains(pick.report, "hot@a: load 2.25, 100.0 GiB free, 1 slot(s) free") {
		t.Fatalf("report = %q, want hot@a listed with its load and free slot", pick.report)
	}
}

func TestPickHostAllOverloadedRanksByLoad(t *testing.T) {
	hosts := []string{"hot@a", "hot@b"}
	probe := fakeProbe(t, map[string]hostStatus{
		"hot@a": {nproc: 2, load1: 8.0, freeGiB: 100, freeSlots: 1},
		"hot@b": {nproc: 4, load1: 12.0, freeGiB: 100, freeSlots: 1},
	}, nil)
	pick, err := pickHost(t.Context(), hosts, probe)
	if err != nil {
		t.Fatalf("overloaded hosts with free slots still qualify, got %v", err)
	}
	if pick.chosen != "hot@b" {
		t.Fatalf("chosen = %q, want hot@b: 3.00 load per CPU beats hot@a at 4.00", pick.chosen)
	}
}

// TestSSHHookRunnerBusyPoolFailsWithoutReusingSlot holds every worktree lock, as four concurrent runs would.
// The remote script must not reuse a busy slot: the run fails as a tool failure and runs no hook.
func TestSSHHookRunnerBusyPoolFailsWithoutReusingSlot(t *testing.T) {
	f, runner := sshGateFixture(t)
	root := filepath.Join(os.Getenv("HOME"), "rjc-hook", "example", "app")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for w := range 4 {
		// Each holder takes its slot lock, prints a line once it holds it, then blocks reading stdin until the test
		// closes it. No sleep: the readiness line is the signal, and closing stdin ends the holder without an orphan.
		holder := exec.Command("flock", filepath.Join(root, "w"+strconv.Itoa(w)+".lock"), "sh", "-c", `echo locked; read _`)
		stdin, err := holder.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout, err := holder.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := holder.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = stdin.Close()
			_ = holder.Wait()
		})
		// Block until this holder reports it owns its slot. No timer: the line is the signal.
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if err != nil || strings.TrimSpace(line) != "locked" {
			t.Fatalf("slot %d holder did not report its lock: %q err=%v", w, line, err)
		}
	}
	f.commit(t, "hk.pkl")
	base, head := sshRange(t, f)
	out, _, err := runner.Run(f.ctx, base, head, nil, io.Discard)
	fail, ok := errors.AsType[*failure](err)
	if !ok || fail.stage != "hook-ssh" || !strings.Contains(err.Error(), "exit status 76") {
		t.Fatalf("busy pool error = %v, want a hook-ssh tool failure from the remote pool-busy exit 76", err)
	}
	if out.Phase == phaseCompleted {
		t.Fatalf("a busy pool must record no hook outcome, got %+v", out)
	}
	if _, statErr := os.Stat(filepath.Join(root, "w0", ".devenv")); statErr == nil {
		t.Fatal("a busy slot was reused: w0 was touched while its lock was held")
	}
}

func TestRunnerSelectionNamesHostAndLoads(t *testing.T) {
	pick := hostPick{chosen: "idle@b", report: []string{"busy@a: load 0.80, 60.0 GiB free", "idle@b: load 0.10, 60.0 GiB free"}}
	want := "hooks: runner=ssh host=idle@b hosts=[busy@a: load 0.80, 60.0 GiB free; idle@b: load 0.10, 60.0 GiB free]"
	if got := runnerSelection(pick); got != want {
		t.Fatalf("selection = %q, want %q", got, want)
	}
	if got := runnerSelection(hostPick{}); got != "hooks: runner=local" {
		t.Fatalf("local selection = %q", got)
	}
}

func TestParseHostStatusReadsFourLines(t *testing.T) {
	// nproc, load, free KiB, free gate slots: the probe prints one line each.
	status, err := parseHostStatus("8\n1.50\n41943040\n2\n")
	if err != nil {
		t.Fatal(err)
	}
	if status.nproc != 8 || status.load1 != 1.5 || status.freeGiB != 40 || status.freeSlots != 2 {
		t.Fatalf("status = %+v, want nproc 8, load 1.5, 40 GiB free, 2 slots free", status)
	}
	if _, err := parseHostStatus("8\n1.50\n41943040\n"); err == nil {
		t.Fatal("three lines, with no slot count, must be rejected")
	}
}

// TestSSHHookRunnerUsesHostHKWithoutBuilding runs with hk on the host PATH. The shell then gets hk from the
// host, and nix build must not run to find it.
func TestSSHHookRunnerUsesHostHKWithoutBuilding(t *testing.T) {
	f, runner := sshGateFixture(t)
	f.commit(t, "hk.pkl")
	base, head := sshRange(t, f)
	if _, _, err := runner.Run(f.ctx, base, head, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(os.Getenv("RJC_TEST_NIX_LOG"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(log), "build --no-link") {
		t.Fatalf("hk is on the host PATH, so nix build must not run; nix log: %q", log)
	}
}

// TestSSHHookRunnerFallbackIncludesConfiguredTool checks the builtin shell package set and configured host runner.
func TestSSHHookRunnerFallbackIncludesConfiguredTool(t *testing.T) {
	for _, tc := range []struct{ name, config, tool string }{
		{name: "lefthook", config: "lefthook.yml", tool: "lefthook"},
		{name: "pre-commit", config: ".pre-commit-config.yaml", tool: "pre-commit"},
		{name: "prek", config: "prek.toml", tool: "prek"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, runner := sshGateFixture(t)
			if err := os.Remove(filepath.Join(f.repo.root, "devenv.nix")); err != nil {
				t.Fatal(err)
			}
			path := hostPathWithout(t, tc.tool)
			if tc.tool == "pre-commit" || tc.tool == "prek" {
				nodeTools := t.TempDir()
				writeFakeTool(t, filepath.Join(nodeTools, "node"), "#!/bin/sh\nexit 0\n")
				writeFakeTool(t, filepath.Join(nodeTools, "npm"), "#!/bin/sh\nexit 0\n")
				path = nodeTools + string(os.PathListSeparator) + path
			}
			f.repo.env = append(f.repo.env, "PATH="+path)
			writeFakeTool(t, filepath.Join(os.Getenv("HOME"), "bin", tc.tool), fakeHK)
			f.commit(t, tc.config)
			base, head := sshRange(t, f)
			var output bytes.Buffer
			out, _, err := runner.Run(f.ctx, base, head, nil, &output)
			if err != nil || out.Phase != phaseCompleted || out.Exit != 0 {
				t.Fatalf("fallback shell: %+v err=%v output=%s", out, err, output.String())
			}
			log, err := os.ReadFile(os.Getenv("RJC_TEST_NIX_LOG"))
			if err != nil {
				t.Fatal(err)
			}
			var shellLine string
			for line := range strings.SplitSeq(string(log), "\n") {
				if strings.Contains(line, " shell ") {
					shellLine = line
					break
				}
			}
			if !strings.Contains(shellLine, hookshell.Nixpkgs+"#"+tc.tool+" -c") {
				t.Fatalf("builtin shell must include configured runner, got nix log %q", log)
			}
			for _, pkg := range []string{"pkl", "git", "nodejs_24", "pnpm", "bun"} {
				if !strings.Contains(shellLine, hookshell.Nixpkgs+"#"+pkg) {
					t.Errorf("builtin shell line lacks %s: %q", pkg, shellLine)
				}
			}
			build := "build --no-link --print-out-paths " + hookshell.Nixpkgs + "#" + tc.tool
			if strings.Contains(string(log), build) {
				t.Fatalf("host %s must not be built, got nix log %q", tc.tool, log)
			}
		})
	}
}

func TestSSHHookRunnerPicksFlakeDevShell(t *testing.T) {
	f, runner := sshGateFixture(t)
	if err := os.Remove(filepath.Join(f.repo.root, "devenv.nix")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(f.repo.root, "flake.nix"), "{}\n")
	f.commit(t, "hk.pkl")
	base, head := sshRange(t, f)
	if _, _, err := runner.Run(f.ctx, base, head, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(os.Getenv("RJC_TEST_NIX_LOG"))
	if err != nil || !strings.Contains(string(log), "develop") {
		t.Fatalf("flake tree must use nix develop, got %q err=%v", log, err)
	}
}

func TestSSHHookScriptUsesHostJujutsuWhenPresent(t *testing.T) {
	script := sshHookScript(exampleDevenvNixpkgs)
	present := "if command -v jj >/dev/null 2>&1; then\n  if PATH=\"$PATH${hook_path:+:$hook_path}\" $shell_prefix sh -c"
	if !strings.Contains(script, present) {
		t.Fatalf("script must run hk directly when the host has jj, got:\n%s", script)
	}
}

func TestSSHHookScriptPinsEveryNixpkgsReference(t *testing.T) {
	script := sshHookScript(exampleDevenvNixpkgs)
	for _, pkg := range []string{"pkl", "git", "nodejs_24", "pnpm", "bun", "jujutsu"} {
		ref := hookshell.Nixpkgs + "#" + pkg
		if !strings.Contains(script, ref) {
			t.Errorf("script lacks pinned package ref %q", ref)
		}
	}
	if !strings.Contains(script, hookshell.Nixpkgs+"#$kind") {
		t.Errorf("script lacks pinned dynamic hook runner package ref")
	}
	if strings.Contains(script, "nixpkgs#") {
		t.Errorf("script contains an unpinned nixpkgs package ref")
	}
}

func TestSSHHookScriptWrapsJujutsuOnlyWhenAbsent(t *testing.T) {
	script := strings.ReplaceAll(sshHookScript(exampleDevenvNixpkgs), `'\''`, `'`)
	absent := "else\n  if PATH=\"$PATH${hook_path:+:$hook_path}\" nix --extra-experimental-features 'nix-command flakes' shell " + hookshell.Nixpkgs + "#jujutsu -c $shell_prefix sh -c"
	if !strings.Contains(script, absent) {
		t.Fatalf("script must wrap hk in nix shell nixpkgs#jujutsu only when the host lacks jj, got:\n%s", script)
	}
}

// TestSSHHookRunnerTimeoutIsToolFailure runs against an ssh that never returns. Expiry must report
// hook-ssh-timeout as a tool failure and record no outcome.
func TestSSHHookRunnerTimeoutIsToolFailure(t *testing.T) {
	f, runner := sshGateFixture(t)
	// Block on a fifo that nothing writes to: a builtin read in the shell, so the timeout's kill ends the wait with no child process.
	fifo := filepath.Join(t.TempDir(), "hang")
	if out, err := exec.Command("mkfifo", fifo).CombinedOutput(); err != nil {
		t.Fatalf("mkfifo: %v\n%s", err, out)
	}
	// Only the hook transfer hangs. The host probe must answer, or the timeout would fire in the probe instead.
	writeFakeTool(t, filepath.Join(os.Getenv("HOME"), "bin", "ssh"), "#!/bin/sh\ncase \"$*\" in\n  *RJC_HOOK_SSH_SHELL*) read _ < "+fifo+" ;;\n  *loadavg*) cat >/dev/null; printf '4\\n0.1\\n104857600\\n1\\n' ;;\n  *) cat >/dev/null ;;\nesac\n")
	t.Setenv("RJC_HOOK_SSH_TIMEOUT", "2s")
	f.commit(t, "hk.pkl")
	base, head := sshRange(t, f)
	out, _, err := runner.Run(f.ctx, base, head, nil, io.Discard)
	fail, ok := errors.AsType[*failure](err)
	if !ok || fail.stage != "hook-ssh-timeout" {
		t.Fatalf("expired run error = %v, want a tool failure with stage hook-ssh-timeout", err)
	}
	if out.Phase == phaseCompleted {
		t.Fatalf("an expired run must record no outcome, got %+v", out)
	}
}

func TestSSHHookRunnerRejectsBadTimeout(t *testing.T) {
	f, runner := sshGateFixture(t)
	t.Setenv("RJC_HOOK_SSH_TIMEOUT", "soon")
	f.commit(t, "hk.pkl")
	base, head := sshRange(t, f)
	_, _, err := runner.Run(f.ctx, base, head, nil, io.Discard)
	fail, ok := errors.AsType[*failure](err)
	if !ok || fail.gate != "hook-ssh-config" {
		t.Fatalf("bad RJC_HOOK_SSH_TIMEOUT error = %v, want hook-ssh-config", err)
	}
}

func TestSSHHookRunnerOverrideShellWins(t *testing.T) {
	f, runner := sshGateFixture(t)
	t.Setenv("RJC_HOOK_SSH_SHELL", "nix shell --")
	f.commit(t, "hk.pkl")
	base, head := sshRange(t, f)
	if _, _, err := runner.Run(f.ctx, base, head, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(os.Getenv("RJC_TEST_NIX_LOG"))
	if err != nil || !strings.Contains(string(log), "shell") || strings.Contains(string(log), "develop") {
		t.Fatalf("RJC_HOOK_SSH_SHELL must replace the tree-picked shell, got %q err=%v", log, err)
	}
}

// reaperBlock extracts the shipped tmp reaper from the remote script, so the test runs the real code, not a copy.
// Its run-dir root is redirected to root, so the test never touches the real $HOME/rjc-hook.
func reaperBlock(t *testing.T, root string) string {
	t.Helper()
	script := sshHookRun
	start := strings.Index(script, `for old in "$HOME/rjc-hook/tmp/rjc-hook."*; do`)
	end := strings.Index(script, "cat > \"$d/in.bundle\"")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("reaper block not found in the remote setup script")
	}
	return strings.ReplaceAll(script[start:end], `"$HOME/rjc-hook/tmp/`, `"`+root+`/`)
}

// TestRunDirFailureExitsBeforeLock proves a run whose mktemp fails exits 78 and never opens a lock. Before the fix the
// empty dir variable made the lock open /.lock, which is not writable, so the script failed with a misleading error.
func TestRunDirFailureExitsBeforeLock(t *testing.T) {
	root := t.TempDir()
	// Make root read-only so mktemp inside it fails.
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	start := strings.Index(sshHookSetup, `d=$(mktemp -d "$HOME/rjc-hook/tmp/rjc-hook.XXXXXX")`)
	end := strings.Index(sshHookSetup, `flock -n -o "$d/.lock"`)
	if start < 0 || end <= start {
		t.Fatal("run directory setup not found before lock")
	}
	script := strings.ReplaceAll(sshHookSetup[start:end], `"$HOME/rjc-hook/tmp/`, `"`+root+`/`) + "\necho lock-reached\n"
	out, err := exec.CommandContext(t.Context(), "sh", "-c", script).CombinedOutput()
	exit, ok := errors.AsType[*exec.ExitError](err)
	if !ok || exit.ExitCode() != 78 {
		t.Fatalf("run dir failure must exit 78, got err=%v out=%q", err, out)
	}
	if !strings.Contains(string(out), "hook-ssh-tmp-failed") {
		t.Fatalf("failure must name hook-ssh-tmp-failed, got %q", out)
	}
	if strings.Contains(string(out), "lock-reached") {
		t.Fatal("run directory failure reached the lock")
	}
}

func TestSSHHookReleasesLocksWithDaemon(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFakeTool(t, filepath.Join(bin, "git"), "#!/bin/sh\ncase \"$*\" in *'worktree add'*) mkdir -p \"$6\"; touch \"$6/hk.pkl\" ;; esac\nexit 0\n")
	writeFakeTool(t, filepath.Join(bin, "jj"), "#!/bin/sh\nexit 0\n")
	writeFakeTool(t, filepath.Join(bin, "hk"), "#!/bin/sh\nfor lock in \"$HOME/rjc-hook/tmp/\"rjc-hook.*/.lock \"$HOME/rjc-hook/slots/s1\" \"$HOME/rjc-hook/owner/repo/w0.lock\"; do if flock -n \"$lock\" true; then echo unlocked; else echo held; fi; done > \"$HOME/locks\"\nsleep 600 &\nprintf '%s' \"$!\" > \"$HOME/pid\"\necho hook-output\nexit 23\n")
	script := strings.Replace(sshHookScript(exampleDevenvNixpkgs), `trap 'rm -rf "$d"' EXIT HUP INT TERM`, ":", 1)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", script, "rjc-hook", "base", "head", "0", "owner", "repo", "1", "hk", "hk.pkl", "30s")
	cmd.WaitDelay = time.Second
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "RJC_HOOK_SSH_SHELL=env")
	out, runErr := cmd.CombinedOutput()
	pidBytes, err := os.ReadFile(filepath.Join(home, "pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(pidBytes))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		process, err := os.FindProcess(pid)
		if err != nil {
			t.Errorf("find hook child: %v", err)
			return
		}
		if err := process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("stop hook child: %v", err)
		}
	})
	if runErr != nil || !bytes.Contains(out, []byte("hook-output")) {
		t.Fatalf("hook script: %v\n%s", runErr, out)
	}
	if err := exec.Command("kill", "-0", strconv.Itoa(pid)).Run(); err == nil {
		status, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		if err != nil || !strings.HasPrefix(strings.TrimSpace(string(status)), "Z") {
			t.Fatalf("hook daemon %d is still running: %q, %v", pid, status, err)
		}
	}
	locks, err := os.ReadFile(filepath.Join(home, "locks"))
	if err != nil || string(locks) != strings.Repeat("held\n", 3) {
		t.Fatalf("locks while hook runs = %q, %v", locks, err)
	}
	dirs, err := filepath.Glob(filepath.Join(home, "rjc-hook", "tmp", "rjc-hook.*"))
	if err != nil || len(dirs) != 1 {
		t.Fatalf("run directory: %v, %v", dirs, err)
	}
	exit, err := os.ReadFile(filepath.Join(dirs[0], "exit"))
	if err != nil || string(exit) != "23\n" {
		t.Fatalf("hook exit = %q, err %v; want 23", exit, err)
	}
	for _, lock := range []string{filepath.Join(dirs[0], ".lock"), filepath.Join(home, "rjc-hook", "slots", "s1"), filepath.Join(home, "rjc-hook", "owner", "repo", "w0.lock")} {
		if out, err := exec.CommandContext(t.Context(), "flock", "-n", lock, "true").CombinedOutput(); err != nil {
			t.Errorf("lock %s remains held: %v %s", lock, err, out)
		}
	}
}

// TestTmpReaperKeepsLiveRunDir proves a reaper cannot delete a locked run directory.
func TestTmpReaperKeepsLiveRunDir(t *testing.T) {
	root := t.TempDir()
	live := filepath.Join(root, "rjc-hook.live")
	stale := filepath.Join(root, "rjc-hook.stale")
	for _, d := range []string{live, stale} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, ".lock"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A long eval leaves the live dir's mtime old. An mtime gate would delete it, so only the lock may protect it.
	old := time.Now().Add(-6 * time.Hour)
	if err := os.Chtimes(live, old, old); err != nil {
		t.Fatal(err)
	}
	// Hold the live run's lock until the reaper has finished; closing stdin releases it.
	holder := exec.CommandContext(t.Context(), "flock", "-x", filepath.Join(live, ".lock"), "-c", "read _")
	stdin, err := holder.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := stdin.Close(); err != nil {
			t.Errorf("close lock holder stdin: %v", err)
		}
		_ = holder.Wait()
	})
	// Run the reaper while another run holds its lock.
	script := reaperBlock(t, root)
	if out, err := exec.CommandContext(t.Context(), "sh", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("reaper: %v\n%s", err, out)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatal("the reaper deleted a live run's dir, the bug this guards against")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("the reaper must remove a dir no run holds")
	}
}

// TestSSHHookRunnerRejectsQuotedShellOverride proves a quoted RJC_HOOK_SSH_SHELL fails before ssh runs. The remote
// shell splits the value on whitespace with no quoting, so a quote can only break the command.
func TestSSHHookRunnerRejectsQuotedShellOverride(t *testing.T) {
	f, runner := sshGateFixture(t)
	argsFile := filepath.Join(t.TempDir(), "ssh-args")
	t.Setenv("RJC_TEST_SSH_ARGS", argsFile)
	t.Setenv("RJC_HOOK_SSH_SHELL", `nix shell "x"`)
	f.commit(t, "hk.pkl")
	base, head := sshRange(t, f)
	_, _, err := runner.Run(f.ctx, base, head, nil, io.Discard)
	fail, ok := errors.AsType[*failure](err)
	if !ok || fail.stage != "hook-ssh" || !strings.Contains(err.Error(), "supports no quoting") {
		t.Fatalf("a quoted shell override must fail with a clear hook-ssh error, got %v", err)
	}
	// Read-only probes run ssh first, legitimately. The contract is that the quoted override never reaches the host.
	if args, statErr := os.ReadFile(argsFile); statErr == nil && strings.Contains(string(args), "nix shell") {
		t.Fatal("the quoted shell override reached the host, the rejection must happen before the transfer")
	}
}

func TestSSHHookRunnerSendsOnlyMissingCommits(t *testing.T) {
	f, runner := sshGateFixture(t)
	f.commit(t, "hk.pkl")
	base, first := sshRange(t, f)
	payload := filepath.Join(t.TempDir(), "payload")
	t.Setenv("RJC_TEST_SSH_PAYLOAD", payload)
	if _, _, err := runner.Run(f.ctx, base, first, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	fullInfo, err := os.Stat(payload)
	if err != nil {
		t.Fatal(err)
	}
	f.commit(t, "second.txt")
	_, second := sshRange(t, f)
	if _, _, err := runner.Run(f.ctx, base, second, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	incrInfo, err := os.Stat(payload)
	if err != nil {
		t.Fatal(err)
	}
	if incrInfo.Size() >= fullInfo.Size() {
		t.Fatalf("second run sent %d bytes, want fewer than the first run's %d", incrInfo.Size(), fullInfo.Size())
	}
}

func TestSSHHookRunnerFallsBackToFullBundle(t *testing.T) {
	f, runner := sshGateFixture(t)
	f.commit(t, "hk.pkl")
	base, first := sshRange(t, f)
	if _, _, err := runner.Run(f.ctx, base, first, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	f.commit(t, "second.txt")
	_, second := sshRange(t, f)
	stale := filepath.Join(t.TempDir(), "stale")
	t.Setenv("RJC_TEST_SSH_STALE_MARK", stale)
	out, _, err := runner.Run(f.ctx, base, second, nil, io.Discard)
	if _, statErr := os.Stat(stale); statErr != nil {
		t.Fatalf("the host never returned the full-bundle status, so the fallback was not exercised: %v", statErr)
	}
	if err != nil || out.Phase != phaseCompleted || out.Exit != 0 {
		t.Fatalf("full-bundle retry: %+v err=%v", out, err)
	}
}

func TestSSHHookRunnerKeepsDevenvAcrossRuns(t *testing.T) {
	f, runner := sshGateFixture(t)
	f.commit(t, "hk.pkl")
	base, head := sshRange(t, f)
	t.Setenv("RJC_TEST_HK_DEVENV", "1")
	for range 2 {
		if _, _, err := runner.Run(f.ctx, base, head, nil, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	marker := filepath.Join(os.Getenv("HOME"), "rjc-hook", "example", "app", "w0", ".devenv", "marker")
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "warm") != 2 {
		t.Fatalf(".devenv must survive between runs in the same worktree, marker=%q", data)
	}
}
