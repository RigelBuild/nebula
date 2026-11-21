package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/RigelBuild/nebula/tools/rjc/internal/hookshell"
)

// maxSSHResult bounds each result file copied back from the remote host.
const maxSSHResult = 1 << 20

var (
	sshHost      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@:\[\]-]*$`)
	sshCachePart = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// sshNeedFullBundle is the remote exit status when the host lacks a prerequisite of an incremental bundle.
const sshNeedFullBundle = 75

// errNeedFullBundle reports that the host cannot take an incremental bundle.
var errNeedFullBundle = errors.New("host lacks bundle prerequisites")

// sshSlotBusy is the remote exit status when this run lost the race for every gate slot. The client waits and re-picks.
const sshSlotBusy = 77

// sshMissingConfig means the reconstructed tree lacks the client-selected config.
const sshMissingConfig = 79

var errMissingConfig = errors.New("remote hook config missing")

const sshMissingRunner = 80

var errMissingRunner = errors.New("remote hook runner missing")

// sshRemoteTimeout signals that the remote timeout expired, not a hook failure.
const sshRemoteTimeout = 81

// timeoutExit and timeoutKilledExit are timeout(1)'s statuses when the client deadline ends the whole remote command,
// setup included: 124 after TERM, 137 when the kill grace ran out.
const (
	timeoutExit       = 124
	timeoutKilledExit = 137
)

// sshSessionReap defines reap <sid>: TERM every process in session sid, then KILL any left after a grace period.
const sshSessionReap = `sweep() {
  p=$(ps -e -o pid= -o sid= | awk -v s="$1" '$2 == s { print $1 }')
  [ -n "$p" ] || return 1
  kill -"$2" $p 2>/dev/null || :
}
reap() { if sweep "$1" TERM; then sleep 5; sweep "$1" KILL || :; fi; }
`

// sshHookWatchdog runs as sh -c <watchdog> rjc-dog <sid> in its own session, with the ssh stdin as its stdin.
// It waits for stdin EOF, then reaps session sid. TERM stops it without a reap.
const sshHookWatchdog = sshSessionReap + `exec 5<&0 </dev/null
cat <&5 >/dev/null & c=$!
exec 5<&-
trap 'kill "$c" 2>/dev/null; exit 0' TERM
wait "$c"
reap "$1"
`

// sshHookGuard wraps the whole remote run: sh -c <guard> rjc-guard <limit> <script> <args...>.
// Stdin carries "<size>\n", the bundle, then stays open until the client ends. The run gets its own session under
// timeout. When stdin closes (the client died or was cancelled) or the run ends, every process left in that session,
// including hook process groups and their builds, gets TERM, then KILL after a grace period.
// The watchdog has a session of its own too: an ssh server such as Tailscale SSH kills the session's process group on
// disconnect, which ends this guard, and the watchdog must outlive that to reap the run.
var sshHookGuard = "dog=" + shQuote(sshHookWatchdog) + "\n" + sshSessionReap + `limit=$1; shift
mkdir -p "$HOME/rjc-hook/tmp"
b=$(mktemp "$HOME/rjc-hook/tmp/in.XXXXXX") || { echo hook-ssh-tmp-failed >&2; exit 78; }
trap 'rm -f "$b"' EXIT
IFS= read -r n || exit 1
case "$n" in ''|*[!0-9]*) exit 1 ;; esac
head -c "$n" > "$b"
[ "$(wc -c < "$b")" -eq "$n" ] || exit 1
exec 4<&0
setsid timeout --kill-after=30s "$limit" sh -c "$@" < "$b" 4<&- &
run=$!
setsid sh -c "$dog" rjc-dog "$run" <&4 >/dev/null 2>&1 &
dog=$!
exec 4<&-
if wait "$run"; then rc=0; else rc=$?; fi
kill "$dog" 2>/dev/null || :
reap "$run"
exit "$rc"
`

// sshDeadlineMargin keeps the remote limit under the client deadline, so the remote timeout fires first.
const sshDeadlineMargin = 5 * time.Second

// errSlotBusy reports a slot race or a queue admission refusal.
var errSlotBusy = errors.New("host gate slot was taken before the run started")

// sshProbeScript prints the commit IDs the host cache holds, from its hook refs. Arg 1 is owner/repo.
const sshProbeScript = `cache="$HOME/rjc-hook/$1.git"
[ -d "$cache" ] || exit 0
git --git-dir="$cache" for-each-ref --sort=-committerdate --count=64 --format='%(objectname)' refs/rjc-hook/hook/
`

// sshHookSetup runs on the host as: sh -c <script> rjc-hook <base> <head> <reports> <owner> <repo>, with the git payload on stdin.
// The host keeps a bare cache per repo and a pool of worktrees, each claimed with flock so .devenv and the moon cache persist.
// Hook output goes to stderr; a tar of results goes to stdout. Exit 75 asks the client for a full bundle.
const sshHookSetup = `set -e
exec 3>&1 1>&2
root="$HOME/rjc-hook/$4/$5"
cache="$root.git"
mkdir -p "$HOME/rjc-hook/$4"
[ -d "$cache" ] || git init -q --bare "$cache"

mkdir -p "$HOME/rjc-hook/tmp"
d=$(mktemp -d "$HOME/rjc-hook/tmp/rjc-hook.XXXXXX") || { echo hook-ssh-tmp-failed >&2; exit 78; }
trap 'rm -rf "$d"' EXIT HUP INT TERM
flock -n -o "$d/.lock" sh -c "$run_script" sh "$d" "$root" "$cache" "$1" "$2" "$3" "$6" "$slot_script" "$body_script" "$hook_script" "$7" "$8" "$9"
`

var sshHookRun = `set -e
d=$1; root=$2; cache=$3; base=$4; head=$5; reports=$6; slots=$7
host_slots=${RJC_HOOK_SSH_HOST_SLOTS:-}
case "$host_slots" in
  ''|*[!0-9]*) ;;
  *) if [ "$host_slots" -gt 0 ] && [ "$host_slots" -lt "$slots" ]; then slots=$host_slots; fi ;;
esac
export RJC_HOOK_SSH_SLOTS="$slots"
slot_script=$8; body_script=$9; shift 9; hook_script=$1; kind=$2; config=$3; limit=$4
ticket=${RJC_HOOK_TICKET:-}
for path in "$HOME/rjc-hook/queue/"*-"$ticket"; do
  [ -n "$ticket" ] && [ -f "$path" ] || continue
  touch -c "$path"
  (while :; do sleep ` + strconv.FormatInt(int64(slotPollInterval/time.Second), 10) + ` & sleeper=$!; trap 'kill "$sleeper" 2>/dev/null; exit' TERM; wait "$sleeper" || exit; [ -d "$d" ] && [ -f "$path" ] || exit; touch -c "$path"; done) 3>&- >/dev/null & heartbeat=$!
  trap 'kill "$heartbeat" 2>/dev/null || :; wait "$heartbeat" 2>/dev/null || :' EXIT
  break
done
for old in "$HOME/rjc-hook/tmp/rjc-hook."*; do
  [ -d "$old" ] && [ -e "$old/.lock" ] || continue
  exec 6>"$old/.lock"
  if flock -n 6; then rm -rf "$old"; fi
  exec 6>&-
done
mkdir -p "$d/tmp" && export TMPDIR="$d/tmp"
cat > "$d/in.bundle"
slots_dir="$HOME/rjc-hook/slots"
mkdir -p "$slots_dir"
` + sshQueueGate + `
for s in $(seq 1 "$slots"); do
  if flock -n -o -E 77 "$slots_dir/s$s" sh -c "$slot_script" sh "$d" "$root" "$cache" "$base" "$head" "$reports" "$body_script" "$hook_script" "$kind" "$config" "$limit"; then exit 0; else rc=$?; fi
  [ "$rc" = 77 ] || exit "$rc"
done
echo hook-ssh-slot-busy >&2; exit 77
`

// sshHookSlot holds a gate slot through the run; 77 asks the client to re-pick.
const sshHookSlot = `set -e
flock -u 8 2>/dev/null || :
d=$1; root=$2; cache=$3; base=$4; head=$5; reports=$6; body_script=$7; hook_script=$8; kind=$9
shift 9; config=$1; limit=$2
if [ -s "$d/in.bundle" ]; then
  git --git-dir="$cache" bundle verify "$d/in.bundle" >/dev/null 2>&1 || exit 75
  git --git-dir="$cache" bundle unbundle "$d/in.bundle" >/dev/null
fi
git --git-dir="$cache" cat-file -e "$head^{commit}" 2>/dev/null || exit 75
if [ -n "${queue_file:-}" ]; then rm -f "$queue_file"; fi
git --git-dir="$cache" update-ref "refs/rjc-hook/hook/$head" "$head"
mkdir -p "$root"
for w in 0 1 2 3; do
  slot="$root/w$w"
  if flock -n -o -E 76 "$slot.lock" sh -c "$body_script" sh "$base" "$head" "$reports" "$slot" "$d" "$cache" "$hook_script" "$kind" "$config" "$limit"; then exit 0; else rc=$?; fi
  [ "$rc" = 76 ] || exit "$rc"
done
echo hook-ssh-pool-busy >&2; exit 76
`

// sshHookBody runs under the worktree lock, held only by its flock parent.
const sshHookBody = `set -e
slot=$4; d=$5; cache=$6
if [ ! -e "$slot/.git" ]; then
  git --git-dir="$cache" worktree prune
  git --git-dir="$cache" worktree add --detach -f "$slot" "$2" >/dev/null
fi
# The flock holder is the only git process that can own this slot, so index.lock and HEAD.lock here are stale.
gitdir=$(git -C "$slot" rev-parse --absolute-git-dir)
rm -f "$gitdir/index.lock" "$gitdir/HEAD.lock"
git -C "$slot" -c advice.detachedHead=false checkout -q -f --detach "$2"
git -C "$slot" clean -ffdx -e .devenv -e .moon/cache -e node_modules
git -C "$slot" reset -q --hard "$2"
cd "$slot"
kind=$8; config=$9; limit=${10}
[ -f "$config" ] || { echo "remote tree lacks $config" >&2; exit 79; }
export MOON_BASE="$1" MOON_HEAD="$2" RJC_MOON_REPORT="$d/moon.json" RJC_MOON_TARGETS=
if [ "$3" = 1 ]; then export RJC_GATE_REPORT="$d/gates.json"; else unset RJC_GATE_REPORT; fi
`

const sshHookTail = `if [ -n "$(git -C "$slot" status --porcelain --untracked-files=no)" ]; then echo hook-mutated-tree >&2; rc=1; fi
printf '%s\n' "$rc" > "$d/exit"
if [ "$3" = 1 ]; then tar -C "$d" -cf - exit moon.json gates.json >&3; else tar -C "$d" -cf - exit >&3; fi
`

//nolint:dupword // The "then ... fi" in the shell script is syntax, not a repeated word.
func sshHookScript(devenvNixpkgs string) string {
	body := sshHookBody + hookshell.Select(devenvNixpkgs) + `hook_path=""
hook_dir=$(command -v "$kind" 2>/dev/null | sed 's#/[^/]*$##')
if [ -z "$hook_dir" ]; then
  if [ "$kind" = hk ]; then
    if [ -z "$shell_managed" ]; then
` + hookshell.Fetch() + `    fi
  else
    hook_dir=$(nix --extra-experimental-features 'nix-command flakes' build --no-link --print-out-paths "` + hookshell.Nixpkgs + `#$kind" | sed 's#$#/bin#' | paste -sd: -)
  fi
fi
if { [ "$kind" = pre-commit ] || [ "$kind" = prek ]; } && ! { command -v node >/dev/null 2>&1 && command -v npm >/dev/null 2>&1; }; then
  # pre-commit uses system node only when node and npm are on PATH; otherwise nodeenv fetches latest. The selected shell's node keeps PATH precedence: the pinned node is appended after it, so the cached build only supplies node when the shell lacks it.
  node_dir=$(nix --extra-experimental-features 'nix-command flakes' build --no-link --print-out-paths "` + hookshell.Nixpkgs + `#nodejs_24" 2>/dev/null | sed 's#$#/bin#' | paste -sd: -) || node_dir=""
  if [ -n "$node_dir" ]; then hook_dir="${hook_dir:+$hook_dir:}$node_dir"; else echo 'rjc: pinned nodejs_24 build failed; pre-commit may fetch node via nodeenv' >&2; fi
fi
hook_path=$(printf '%s' "$hook_dir" | tr -d '\n')
: > "$d/hook.log"
timeout --kill-after=30s "$limit" sh -c "$7" sh "$1" "$2" "$shell_prefix" "$hook_path" "$kind" "$config" "$d/missing-runner" "$d/hook-completed" >"$d/hook.log" 2>&1 3>&- &
tp=$!
tail --pid="$tp" -n +1 -f "$d/hook.log" >&2 3>&- &
stream=$!
if wait "$tp"; then rc=0; else rc=$?; fi
kill -TERM -- -"$tp" 2>/dev/null || :
wait "$stream"
if [ ! -e "$d/hook-completed" ] && { [ "$rc" = 124 ] || [ "$rc" = 137 ]; }; then echo hook-ssh-timeout >&2; exit 81; fi
if [ -e "$d/missing-runner" ]; then echo "remote shell lacks $kind" >&2; exit 80; fi
` + sshHookTail
	hook := `shell_prefix=$3; hook_path=$4; kind=$5; missing=$7; completed=$8
case "$kind" in
  hk) set -- hk run pre-push --from-ref "$1" --to-ref "$2" ;;
  lefthook) printf 'refs/heads/x %s refs/heads/x %s\n' "$2" "$1" > "$TMPDIR/push-refs"; set -- lefthook run pre-push origin rjc-ssh ;;
  pre-commit) set -- pre-commit run --config "$6" --hook-stage pre-push --from-ref "$1" --to-ref "$2" ;;
  prek) set -- prek run --config "$6" --hook-stage pre-push --from-ref "$1" --to-ref "$2" ;;
  *) echo "unknown hook runner: $kind" >&2; exit 2 ;;
esac
if [ "$kind" = lefthook ]; then exec < "$TMPDIR/push-refs"; fi
check='missing=$1; shift; if ! command -v "$1" >/dev/null 2>&1; then : > "$missing"; exit 1; fi; exec "$@"'
if command -v jj >/dev/null 2>&1; then
  if PATH="$PATH${hook_path:+:$hook_path}" $shell_prefix sh -c "$check" sh "$missing" "$@"; then rc=0; else rc=$?; fi
else
  if PATH="$PATH${hook_path:+:$hook_path}" nix --extra-experimental-features 'nix-command flakes' shell ` + hookshell.Nixpkgs + `#jujutsu -c $shell_prefix sh -c "$check" sh "$missing" "$@"; then rc=0; else rc=$?; fi
fi
: > "$completed"
exit "$rc"
`
	return "run_script=" + shQuote(sshHookRun) + "\nslot_script=" + shQuote(sshHookSlot) +
		"\nbody_script=" + shQuote(body) + "\nhook_script=" + shQuote(hook) + "\n" + sshHookSetup
}

// sshHookRunner runs pre-push hooks on a remote host. The remote tree picks the dev shell.
// The head arrives as a git bundle on ssh stdin, so nothing is pushed.
// host is the one host this run uses. pickRunnerHost chooses it from hosts before the run.
type sshHookRunner struct {
	repo              *repository
	host              string
	hosts             []string
	identity          string
	probe             hostProbe
	ticket            string
	queue             *queueHosts
	enqueue           bool
	// freeGiB is a seam for the free space each probed host reports. Nil means the probed value. Tests inject a fixed value.
	freeGiB func(hostStatus) float64
	heartbeatInterval time.Duration
	// wait and now are seams for the slot queue. Nil means the real timer and clock. Tests use a fake clock.
	wait func(ctx context.Context, d time.Duration) error
	now  func() time.Time
}

func (r *repository) hookRunner() HookRunner {
	hosts := splitHosts(envLookup(r.env, "RJC_HOOK_SSH"))
	if len(hosts) == 0 {
		return localHookRunner{repo: r}
	}
	return sshHookRunner{repo: r, hosts: hosts, identity: envLookup(r.env, "RJC_HOOK_SSH_IDENTITY")}
}

// runnerSelection names the hook runner a run will use, and every probed host, for the run log.
func runnerSelection(pick hostPick) string {
	if pick.chosen == "" {
		return "hooks: runner=local"
	}
	return "hooks: runner=ssh host=" + pick.chosen + " hosts=[" + strings.Join(pick.report, "; ") + "]"
}

// defaultSSHTimeout bounds one whole ssh hook run, so a stalled remote cannot hold a submit forever.
const defaultSSHTimeout = 45 * time.Minute

// defaultSSHSlots is how many hook runs one host takes at once when RJC_HOOK_SSH_SLOTS is unset.
const defaultSSHSlots = 2

// Run runs the hooks on the host under a deadline. RJC_HOOK_SSH_TIMEOUT overrides defaultSSHTimeout.
// On expiry it records nothing and reports hook-ssh-timeout as a tool failure.
func (r sshHookRunner) Run(ctx context.Context, base, head string, skip []SkippedTarget, log io.Writer) (HookOutcome, RunRef, error) {
	timeout, err := r.hookTimeout()
	if err != nil {
		return HookOutcome{}, RunRef{}, err
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		outcome, ref, err := r.run(runCtx, base, head, skip, log)
		if errors.Is(err, errSlotBusy) {
			// Lost the race for the slot after the probe. Re-pick through the queue, bounded by the same deadline.
			picked, _, perr := r.pickRunnerHost(runCtx, log)
			if perr != nil {
				return HookOutcome{}, RunRef{}, perr
			}
			r = picked
			continue
		}
		if err != nil && runCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			return HookOutcome{}, RunRef{}, toolFailure("hook-ssh-timeout", fmt.Errorf("ssh hook run exceeded %s", timeout))
		}
		return outcome, ref, err
	}
}

// clock returns the current time. Tests replace it with a fake clock.
func (r sshHookRunner) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// slotPollInterval is how long a run waits before it probes again while every gate slot is busy.
const slotPollInterval = 30 * time.Second

// pickRunnerHost probes the runner's hosts and returns a copy that uses the chosen one, plus the probe report.
// When every qualifying host has its gate slots busy it waits and re-picks, until RJC_HOOK_SSH_TIMEOUT runs out.
// A single host is still probed, so reachability and free space are checked the same way.
func (r sshHookRunner) pickRunnerHost(ctx context.Context, log io.Writer) (picked sshHookRunner, selection hostPick, retErr error) {
	if r.ticket == "" {
		r.ticket = newQueueTicket()
	}
	if r.queue == nil {
		r.queue = &queueHosts{}
	}
	defer func() {
		if retErr != nil {
			r.cleanupTickets(ctx)
		}
	}()
	probe := r.probe
	if probe == nil {
		probe = r.sshProbe
	}
	wait := r.wait
	if wait == nil {
		wait = waitFor
	}
	timeout, err := r.hookTimeout()
	if err != nil {
		return r, hostPick{}, err
	}
	if _, err := r.hookSlots(); err != nil {
		return r, hostPick{}, err
	}
	deadline := r.clock().Add(timeout)
	for {
		pick, err := pickHost(ctx, r.hosts, probe)
		if err == nil {
			r.host = pick.chosen
			r.cleanupOtherTickets(ctx, r.host)
			return r, pick, nil
		}
		if !errors.Is(err, errNoFreeSlot) {
			return r, pick, toolFailure("hook-ssh-no-host", err)
		}
		if r.probe == nil && !r.enqueue {
			r.enqueue = true
			probe = r.sshProbe
			continue
		}
		if !r.clock().Before(deadline) {
			return r, pick, toolFailure("hook-ssh-timeout", fmt.Errorf("no gate slot freed within %s (hosts: %s)", timeout, strings.Join(pick.report, "; ")))
		}
		if _, werr := fmt.Fprintf(log, "hook-ssh: waiting for a gate slot, position %d of %d (hosts: %s)\n", pick.queuePosition, pick.queueTotal, strings.Join(pick.report, "; ")); werr != nil {
			return r, pick, toolFailure("hooks", werr)
		}
		if err := wait(ctx, slotPollInterval); err != nil {
			return r, pick, toolFailure("hook-ssh-timeout", err)
		}
	}
}

// waitFor blocks for d, or until ctx ends. Tests replace it with a fake clock.
func waitFor(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// sshProbe reads one host's load and free space over ssh. It has a 10 s connect timeout.
func (r sshHookRunner) sshProbe(ctx context.Context, host string) (hostStatus, error) {
	args := []string{"-o", sshBatchMode, "-o", sshConnectTimeout}
	if r.identity != "" {
		args = append(args, "-i", r.identity)
	}
	slots, err := r.hookSlots()
	if err != nil {
		return hostStatus{}, err
	}
	ticket := ""
	if r.enqueue {
		ticket = r.ticket
	}
	args = append(args, "--", host, "sh -c "+shQuote(sshHostProbeScript)+queueProbeArgs(slots, ticket))
	var out bytes.Buffer
	cmd := r.repo.execCommand(ctx, "ssh", args...)
	cmd.Dir, cmd.Env, cmd.Stdout = r.repo.root, r.repo.childEnv(nil), &out
	if err := cmd.Run(); err != nil {
		return hostStatus{}, err
	}
	status, err := parseHostStatus(out.String())
	if err == nil && r.freeGiB != nil {
		status.freeGiB = r.freeGiB(status)
	}
	if err == nil && status.queuePosition > 0 && r.queue != nil {
		r.queue.add(host)
	}
	return status, err
}

func (r sshHookRunner) run(ctx context.Context, base, head string, skip []SkippedTarget, log io.Writer) (_ HookOutcome, _ RunRef, retErr error) {
	var outcome HookOutcome
	if len(skip) != 0 {
		return outcome, RunRef{}, refusal("hooks", "skip-target-unsupported-over-ssh", "selective skips are not supported on the ssh hook runner", "rerun without --skip-target, or unset RJC_HOOK_SSH")
	}
	if !sshHost.MatchString(r.host) {
		return outcome, RunRef{}, refusal("hooks", "hook-ssh-config", "RJC_HOOK_SSH is not a valid ssh host", "set RJC_HOOK_SSH to user@host")
	}
	if !objectID.MatchString(base) || !objectID.MatchString(head) {
		return outcome, RunRef{}, toolFailure("hooks", errors.New("invalid hook range"))
	}
	interval := r.heartbeatInterval
	if interval <= 0 {
		interval = slotPollInterval
	}
	stop := r.heartbeatTicket(ctx, interval)
	defer stop()
	outcome = HookOutcome{From: base, To: head}
	rec, config, err := r.repo.hookIdentityConfig(ctx, Change{Tip: head, Base: base})
	if err != nil {
		return outcome, RunRef{}, err
	}
	if rec.HookConfigBlob == "" {
		outcome.Phase = phaseNoConfig
		return outcome, RunRef{}, nil
	}
	dir, err := os.MkdirTemp("", "rjc-ssh-")
	if err != nil {
		return outcome, RunRef{}, toolFailure("hook-ssh", err)
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(dir)) }()
	ref := "refs/rjc-hook/ssh-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := r.repo.command(ctx, nil, nil, "git", "update-ref", ref, head); err != nil {
		return outcome, RunRef{}, toolFailure("hook-ssh", err)
	}
	defer func() {
		_, err := r.repo.command(context.WithoutCancel(ctx), nil, nil, "git", "update-ref", "-d", ref)
		retErr = errors.Join(retErr, err)
	}()
	have, err := r.haveCommits(ctx, log)
	if err != nil {
		return outcome, RunRef{}, toolFailure("hook-ssh", err)
	}
	payload := filepath.Join(dir, "hook.bundle")
	if err := r.writePayload(ctx, payload, ref, head, have); err != nil {
		return outcome, RunRef{}, toolFailure("hook-ssh", err)
	}
	result, err := r.transfer(ctx, payload, base, head, config.kind, config.path, log)
	if errors.Is(err, errNeedFullBundle) && len(have) != 0 {
		full := filepath.Join(dir, "full.bundle")
		if err := r.makeBundle(ctx, full, ref, nil); err != nil {
			return outcome, RunRef{}, toolFailure("hook-ssh", err)
		}
		result, err = r.transfer(ctx, full, base, head, config.kind, config.path, log)
	}
	if errors.Is(err, errMissingConfig) {
		outcome.Phase = phaseSetupFailed
		return outcome, RunRef{}, toolFailure("hook-ssh", fmt.Errorf("remote tree lacks %s", config.path))
	}
	if errors.Is(err, errMissingRunner) {
		outcome.Phase = phaseSetupFailed
		return outcome, RunRef{}, toolFailure("hook-ssh", fmt.Errorf("remote shell lacks %s", config.kind))
	}
	if fail, ok := errors.AsType[*failure](err); ok && fail.stage == "hook-ssh-timeout" {
		return outcome, RunRef{}, err
	}
	if err != nil {
		return outcome, RunRef{}, toolFailure("hook-ssh", err)
	}
	if err := readSSHResults(result, dir, r.repo.profile.HookReports); err != nil {
		return outcome, RunRef{}, toolFailure("hook-ssh", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "exit")) //nolint:gosec // The path is in a private temporary directory.
	if err != nil {
		return outcome, RunRef{}, toolFailure("hook-ssh", err)
	}
	outcome.Exit, err = strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return outcome, RunRef{}, toolFailure("hook-ssh", fmt.Errorf("invalid remote exit status: %w", err))
	}
	outcome.Phase = phaseCompleted
	if r.repo.profile.HookReports {
		outcome.FailedTargets, err = readMoonReport(filepath.Join(dir, "moon.json"), nil, nil)
		if err != nil {
			return outcome, RunRef{}, toolFailure("hooks", err)
		}
		outcome.FailedGates, err = readGateReport(filepath.Join(dir, "gates.json"))
		if err != nil {
			return outcome, RunRef{}, toolFailure("hooks", err)
		}
	}
	return outcome, RunRef{}, nil
}

// shQuote returns value as one POSIX shell word.
func shQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
