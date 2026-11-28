import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";

const PROBE = `
import os, signal, sys
open_pidfd = getattr(os, "pidfd_open", None)
send_pidfd_signal = getattr(signal, "pidfd_send_signal", None)
if not callable(open_pidfd) or not callable(send_pidfd_signal):
    print("pidfd unsupported: Python or Linux does not expose pidfd_open and pidfd_send_signal", file=sys.stderr)
    raise SystemExit(2)
try:
    pidfd = open_pidfd(os.getpid())
    send_pidfd_signal(pidfd, 0)
except OSError as error:
    print("pidfd unsupported: " + str(error), file=sys.stderr)
    raise SystemExit(2)
finally:
    if 'pidfd' in locals(): os.close(pidfd)
print("supported")
`;

const SIGNAL = `
import errno, json, os, select, signal, sys
pid = int(sys.argv[1])
expected_start, expected_comm = sys.argv[2], sys.argv[3]
markers = [os.fsencode(marker) for marker in json.loads(sys.argv[4])]
open_pidfd = getattr(os, "pidfd_open", None)
send_pidfd_signal = getattr(signal, "pidfd_send_signal", None)
if not callable(open_pidfd) or not callable(send_pidfd_signal):
    print("pidfd unsupported: Python or Linux does not expose pidfd_open and pidfd_send_signal", file=sys.stderr)
    raise SystemExit(2)
try:
    pidfd = open_pidfd(pid)
except ProcessLookupError:
    print("gone")
    raise SystemExit(0)
except OSError as error:
    if error.errno == errno.ENOSYS:
        print("pidfd unsupported: kernel does not implement pidfd_open", file=sys.stderr)
    else:
        print("pidfd_open failed: " + str(error), file=sys.stderr)
    raise SystemExit(2)
try:
    poller = select.poll()
    poller.register(pidfd, select.POLLIN)
    if poller.poll(0):
        print("gone")
        raise SystemExit(0)
    try:
        stat = open(f"/proc/{pid}/stat", encoding="utf-8").read()
        argv = open(f"/proc/{pid}/cmdline", "rb").read().split(b"\\0")
    except OSError as error:
        print("process identity unavailable: " + str(error), file=sys.stderr)
        raise SystemExit(2)
    open_paren = stat.find("(")
    close_paren = stat.rfind(") ")
    fields = stat[close_paren + 2:].split() if close_paren >= 0 else []
    command = stat[open_paren + 1:close_paren] if open_paren >= 0 and close_paren > open_paren else ""
    start = fields[19] if len(fields) > 19 else ""
    if start != expected_start or command != expected_comm or not all(any(marker in arg for arg in argv) for marker in markers):
        print("process identity changed since scan; refusing to signal", file=sys.stderr)
        raise SystemExit(2)
    try:
        send_pidfd_signal(pidfd, signal.SIGKILL)
    except OSError as error:
        if error.errno == errno.ENOSYS:
            print("pidfd unsupported: kernel does not implement pidfd_send_signal", file=sys.stderr)
        else:
            print("pidfd_send_signal failed: " + str(error), file=sys.stderr)
        raise SystemExit(2)
    if not poller.poll(5000):
        print("pidfd signal sent but process did not exit within 5 seconds", file=sys.stderr)
        raise SystemExit(2)
    print("signaled")
finally:
    os.close(pidfd)
`;

/** @typedef {{ supported: boolean, error?: string }} PidfdSupport */
/** @typedef {{ status: "signaled" | "gone" | "failed", error?: string }} PidfdSignalResult */
/** @typedef {{ command: string, startTime: string }} ProcessIdentity */

/** @returns {PidfdSupport} */
export function checkPidfdSupport() {
  if (process.platform !== "linux") return { supported: false, error: `pidfd requires Linux; found ${process.platform}` };
  const result = spawnSync("python3", ["-c", PROBE], { encoding: "utf8", timeout: 10_000 });
  if (result.error) return { supported: false, error: `pidfd unsupported: could not run python3 helper: ${result.error.message}` };
  if (result.status === 0 && result.stdout.trim() === "supported") return { supported: true };
  return { supported: false, error: result.stderr.trim() || `pidfd support probe exited ${result.status}` };
}

/** @param {string} stat @returns {ProcessIdentity | undefined} */
function parseProcessIdentity(stat) {
  const open = stat.indexOf("("), close = stat.lastIndexOf(") ");
  if (open < 0 || close <= open) return undefined;
  const startTime = stat.slice(close + 2).split(/\s+/)[19];
  if (startTime === undefined || !/^\d+$/.test(startTime)) return undefined;
  return { command: stat.slice(open + 1, close), startTime };
}

/**
 * Read Linux process command and start time from one /proc stat record.
 * @param {number} pid
 * @param {{ readStat?: (pid: number) => string }} [options]
 * @returns {ProcessIdentity | undefined}
 */
export function readProcessIdentity(pid, { readStat = (target) => readFileSync(`/proc/${target}/stat`, "utf8") } = {}) {
  if (process.platform !== "linux" || !Number.isSafeInteger(pid) || pid <= 0) return undefined;
  try { return parseProcessIdentity(readStat(pid)); }
  catch { return undefined; }
}

/** @param {import("node:child_process").ChildProcess} child */
function killOwnedChildAndWait(child) {
  return new Promise((resolve) => {
    if (child.exitCode !== null || child.signalCode !== null) return resolve();
    child.once("exit", resolve);
    try { child.kill("SIGKILL"); } catch { /* already exited */ }
    if (child.exitCode !== null || child.signalCode !== null) resolve();
  });
}

/**
 * Record a child identity immediately after spawn. If it cannot be read, stop and reap that exact
 * ChildProcess handle before refusing to continue.
 * @param {Parameters<typeof killOwnedChildAndWait>[0]} child
 * @param {string} marker
 * @param {{ readIdentity?: typeof readProcessIdentity }} [options]
 */
export async function captureSpawnedIdentity(child, marker, { readIdentity = readProcessIdentity } = {}) {
  if (child.pid === undefined) throw new Error("fixture child did not start");
  const identity = readIdentity(child.pid);
  if (!identity?.startTime || !identity.command) {
    await killOwnedChildAndWait(child);
    throw new Error(`fixture child ${child.pid} start identity unavailable; stopped immediately`);
  }
  return { pid: child.pid, marker, command: identity.command, startTime: identity.startTime };
}
/**
 * Recheck a scanned process through pidfd and signal only the pinned identity.
 * `beforeSignal`, `readIdentity`, `runSignal`, and `checkSupport` are injectable regression seams.
 * @param {number} pid
 * @param {ProcessIdentity | undefined} identity
 * @param {string[]} markers
 * @param {{ beforeSignal?: (pid: number) => void, readIdentity?: (pid: number) => ProcessIdentity | undefined, runSignal?: typeof spawnSync, checkSupport?: typeof checkPidfdSupport }} [options]
 * @returns {PidfdSignalResult}
 */
export function signalByPidfd(pid, identity, markers, { beforeSignal, readIdentity = readProcessIdentity, runSignal = spawnSync, checkSupport = checkPidfdSupport } = {}) {
  if (process.platform !== "linux") return { status: "failed", error: `pidfd unsupported on ${process.platform}` };
  if (!Number.isSafeInteger(pid) || pid <= 0 || !identity?.startTime || !/^\d+$/.test(identity.startTime) || !identity.command || markers.length === 0 || markers.some((marker) => !marker)) {
    return { status: "failed", error: "missing process identity; refusing to signal" };
  }
  beforeSignal?.(pid);
  const current = readIdentity(pid);
  if (!current || current.startTime !== identity.startTime || current.command !== identity.command) {
    return { status: "failed", error: "process identity changed since scan; refusing to signal" };
  }
  const support = checkSupport();
  if (!support.supported) return { status: "failed", error: support.error ?? "pidfd unsupported" };
  const result = runSignal("python3", ["-c", SIGNAL, String(pid), identity.startTime, identity.command, JSON.stringify(markers)], {
    encoding: "utf8", timeout: 10_000,
  });
  if (result.error) return { status: "failed", error: `pidfd unsupported: could not run python3 helper: ${result.error.message}` };
  if (result.status === 0 && result.stdout.trim() === "signaled") return { status: "signaled" };
  if (result.status === 0 && result.stdout.trim() === "gone") return { status: "gone" };
  return { status: "failed", error: result.stderr.trim() || `pidfd helper exited ${result.status}` };
}
