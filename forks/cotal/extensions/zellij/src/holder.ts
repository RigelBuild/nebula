import { execFileSync } from "node:child_process";
import { readFileSync, rmSync } from "node:fs";
import {
  registry,
  SpawnRefused,
  type AgentHandle,
  type LaunchSpec,
  type Runtime,
  type RuntimeProvider,
  type RuntimeReference,
} from "@cotal-ai/core";
import { privateLauncher } from "./runtime.js";

const STOP_GRACE_MS = 30_000;
const POLL_MS = 1_000;
const START_TIMEOUT_MS = 15_000;
/** Cap on `waitForWorkExit`, kept below the manager's RESTART_WORK_EXIT_MS (10 s). The holder's diagnostic, which names the
 *  observed state, then fires before the manager's generic timeout, so the log shows the cause. */
const WORK_EXIT_MS = 9_000;

export type HolderReapEvidence =
  | { outcome: "absent" }
  | { outcome: "reaped"; detail: string }
  | { outcome: "retained"; reason: "unprovable" | "members-remain"; detail: string };
type ProcessState = "alive" | "gone" | "unknown";
type ProcessIdentity = { readonly pid: number; readonly startTime: string };
type ProcessRecord = ProcessIdentity & { readonly state: string };
type ExecOptions = { readonly cwd?: string; readonly encoding: "utf8"; readonly timeout?: number };
type HolderExec = (file: string, args: string[], options: ExecOptions) => string;
type ProcFs = { readFileSync(path: string, encoding: "utf8"): string };
type HolderRuntimeOptions = {
  readonly holderCommand?: () => string | undefined;
  readonly unitPrefix?: () => string;
  readonly execFileSync?: HolderExec;
  readonly fs?: ProcFs;
  readonly sleep?: (ms: number) => Promise<void>;
  readonly now?: () => number;
  readonly kill?: (pid: number, signal: NodeJS.Signals) => void;
  /** "darwin" selects wave-host identity and stop; any other value keeps /proc and systemctl. */
  readonly platform?: NodeJS.Platform;
  /** The Darwin wave-host executable. Required on darwin; the seat cannot start without it. */
  readonly waveHostCommand?: () => string | undefined;
};

const defaultExec: HolderExec = (file, args, options) => execFileSync(file, args, options);
const defaultFs: ProcFs = { readFileSync: (path, encoding) => readFileSync(path, encoding) };
const defaultSleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));
const defaultKill = (pid: number, signal: NodeJS.Signals): void => { process.kill(pid, signal); };

function parseProcessStat(raw: string, expectedPid: number): ProcessRecord | undefined {
  const closingParen = raw.lastIndexOf(")");
  const prefix = closingParen < 0 ? undefined : /^(\d+) \(/.exec(raw);
  if (prefix?.[1] !== String(expectedPid) || closingParen < 0) return undefined;
  const fields = raw.slice(closingParen + 1).trim().split(/\s+/u);
  const state = fields[0];
  const startTime = fields[19];
  if (state === undefined || startTime === undefined || !/^\d+$/u.test(startTime)) return undefined;
  return { pid: expectedPid, state, startTime };
}

function isMissingProcess(error: unknown): boolean {
  if (typeof error !== "object" || error === null || !("code" in error)) return false;
  return error.code === "ENOENT" || error.code === "ESRCH";
}

function exitStatus(error: unknown): number | undefined {
  if (typeof error !== "object" || error === null || !("status" in error)) return undefined;
  return typeof error.status === "number" ? error.status : undefined;
}

function parseStartOutput(stdout: string): { holderPid: number; agentPid: number } {
  const match = /^(\d+)\s+(\d+)\s*$/u.exec(stdout);
  const holderPid = match?.[1] === undefined ? undefined : Number(match[1]);
  const agentPid = match?.[2] === undefined ? undefined : Number(match[2]);
  if (!Number.isSafeInteger(holderPid) || holderPid === undefined || holderPid <= 0 ||
      !Number.isSafeInteger(agentPid) || agentPid === undefined || agentPid <= 0)
    throw new Error(`holder runtime: invalid start output ${JSON.stringify(stdout)}`);
  return { holderPid, agentPid };
}

const UNIT_SHOW_ARGS = ["--user", "show", "-p", "LoadState", "-p", "ActiveState", "-p", "ControlGroup", "-p", "ConsistsOf"] as const;
const MEMBER_SHOW_ARGS = ["--user", "show", "-p", "LoadState", "-p", "ActiveState", "-p", "ControlGroup"] as const;
const LIVE_UNIT_STATES: Record<string, true> = { active: true, activating: true, deactivating: true, reloading: true };

type UnitShow = { LoadState: string; ActiveState: string; ControlGroup: string; ConsistsOf: string };

/** Parse `systemctl show` output for the fields above. A unit that is not loaded reads as `LoadState=not-found` and
 *  needs no other field. Any other missing field throws, so a half-read unit never resolves. */
function parseUnitShow(stdout: string, unit: string): UnitShow {
  const fields = new Map(stdout.split("\n").flatMap((line) => {
    const cut = line.indexOf("=");
    return cut < 0 ? [] : [[line.slice(0, cut), line.slice(cut + 1)] as const];
  }));
  const LoadState = fields.get("LoadState");
  if (LoadState === "not-found") return { LoadState, ActiveState: "", ControlGroup: "", ConsistsOf: "" };
  const ActiveState = fields.get("ActiveState");
  const ControlGroup = fields.get("ControlGroup");
  const ConsistsOf = fields.get("ConsistsOf") ?? "";
  if (LoadState === undefined || ActiveState === undefined || ActiveState === "" || ControlGroup === undefined)
    throw new Error(`holder runtime: could not read unit state of ${unit} ${JSON.stringify(stdout)}`);
  return { LoadState, ActiveState, ControlGroup, ConsistsOf };
}

function parseReference(reference: RuntimeReference): { name: string; holder: ProcessIdentity; agent: ProcessIdentity } | undefined {
  if (reference.kind !== "holder") return undefined;
  const match = /^([A-Za-z0-9_.-]+):([1-9]\d*):(\d+):([1-9]\d*):(\d+)$/u.exec(reference.id);
  if (!match?.[1] || !match[2] || !match[3] || !match[4] || !match[5]) return undefined;
  const holderPid = Number(match[2]);
  const agentPid = Number(match[4]);
  if (!Number.isSafeInteger(holderPid) || !Number.isSafeInteger(agentPid)) return undefined;
  return {
    name: match[1],
    holder: { pid: holderPid, startTime: match[3] },
    agent: { pid: agentPid, startTime: match[5] },
  };
}

export class HolderRuntime implements Runtime {
  readonly kind = "holder" as const;
  readonly streamsTerminal = false;
  readonly supportsWorkClosure: boolean;
  private readonly command: () => string | undefined;
  private readonly prefix: () => string;
  private readonly exec: HolderExec;
  private readonly fs: ProcFs;
  private readonly sleep: (ms: number) => Promise<void>;
  private readonly now: () => number;
  private readonly kill: (pid: number, signal: NodeJS.Signals) => void;
  private readonly platform: NodeJS.Platform;
  private readonly waveHost: () => string | undefined;

  constructor(options: HolderRuntimeOptions = {}) {
    this.command = options.holderCommand ?? (() => process.env.COTAL_HOLDER_COMMAND);
    this.prefix = options.unitPrefix ?? (() => process.env.WAVE_SEAT_UNIT_PREFIX ?? "wave-seat-");
    this.exec = options.execFileSync ?? defaultExec;
    this.fs = options.fs ?? defaultFs;
    this.sleep = options.sleep ?? defaultSleep;
    this.now = options.now ?? Date.now;
    this.platform = options.platform ?? process.platform;
    this.waveHost = options.waveHostCommand ?? (() => process.env.WAVE_HOST_COMMAND ?? "wave-host");
    this.kill = options.kill ?? defaultKill;
    this.supportsWorkClosure = this.platform !== "darwin";
  }

  spawn(name: string, spec: LaunchSpec, cwd: string): AgentHandle {
    if (spec.confirm !== undefined)
      throw new SpawnRefused("holder runtime: startup confirmation is not supported");
    const holder = this.command();
    if (holder === undefined)
      throw new SpawnRefused("holder runtime: COTAL_HOLDER_COMMAND is not set");
    // Resolve wave-host before the holder starts a seat: without it the seat would
    // run with no reference and no stop path.
    if (this.platform === "darwin") this.requireWaveHost();

    const launcher = privateLauncher(spec, cwd, name);
    let launcherCleaned = false;
    const cleanupLauncher = (): void => {
      if (launcherCleaned) return;
      launcherCleaned = true;
      rmSync(launcher.dir, { recursive: true, force: true });
    };
    let holderAcceptedStart = false;
    try {
      let stdout: string;
      try {
        stdout = this.exec(holder, ["start", "--name", name, "--", ...launcher.argv], {
          cwd: launcher.cwd,
          encoding: "utf8",
          timeout: START_TIMEOUT_MS,
        });
        holderAcceptedStart = true;
      } catch (error) {
        if (exitStatus(error) === 4)
          throw new SpawnRefused(`seat ${name} is already live; stop it with: systemctl --user stop ${this.prefix()}${name}`);
        throw error;
      }

      const { holderPid, agentPid } = parseStartOutput(stdout);
      const holderRecord = this.readProcess(holderPid);
      const agentRecord = this.readProcess(agentPid);
      if (holderRecord === undefined || agentRecord === undefined ||
          holderRecord.state === "Z" || holderRecord.state === "X" ||
          agentRecord.state === "Z" || agentRecord.state === "X")
        throw new Error(`holder runtime: could not read live process identities for seat ${name}`);
      // Read the agent's run-p scope while its pid is proved live. At wait time the pid is usually gone, and its
      // /proc entry with it, so the omp scope would go unchecked.
      const agentScope = this.agentRunScope(agentPid);

      const reference: RuntimeReference = {
        kind: "holder",
        id: `${name}:${holderPid}:${holderRecord.startTime}:${agentPid}:${agentRecord.startTime}`,
      };
      const identities = {
        holder: { pid: holderPid, startTime: holderRecord.startTime },
        agent: { pid: agentPid, startTime: agentRecord.startTime },
      };
      const status = (): "running" | "exited" => {
        const state = this.status(identities);
        if (state === "exited") cleanupLauncher();
        return state;
      };
      cleanupLauncher();
      return {
        name,
        kind: "holder",
        reference,
        pid: agentPid,
        status,
        stop: (opts) => {
          if (this.areGone(identities)) return;
          try {
            if (opts?.graceful === false) this.forceStop(name, identities);
            else this.stopUnit(name, identities);
          } catch (error) {
            if (!this.areGone(identities)) throw error;
          }
        },
        interrupt: () => this.exec(holder, ["interrupt", "--name", name], { encoding: "utf8", timeout: START_TIMEOUT_MS }),
        waitForExit: async () => {
          while (status() !== "exited") await this.sleep(POLL_MS);
        },
        ...(this.platform === "darwin" ? {} : { waitForWorkExit: () => this.waitForUnitWorkExit(name, agentScope) }),
        attach: () => {
          throw new Error("holder runtime: attach with wave-seat-holder attach");
        },
      };
    } catch (error) {
      if (holderAcceptedStart) {
        try { this.stopSeatJob(name); } catch { /* the original start failure is primary */ }
        try { cleanupLauncher(); } catch { /* best effort after stopping the unit */ }
      }
      throw error;
    } finally {
      if (!holderAcceptedStart) cleanupLauncher();
    }
  }

  async reap(reference: RuntimeReference): Promise<HolderReapEvidence> {
    const parsed = parseReference(reference);
    if (parsed === undefined) return { outcome: "absent" };
    const identities = { holder: parsed.holder, agent: parsed.agent };
    if (this.areGone(identities)) return { outcome: "reaped", detail: `both process identities for seat ${parsed.name} are gone` };

    try {
      this.stopUnit(parsed.name, identities);
    } catch {
      // Still wait and force below; a failed graceful request is not proof of death.
    }
    const deadline = this.now() + STOP_GRACE_MS;
    while (!this.areGone(identities) && this.now() < deadline)
      await this.sleep(Math.min(POLL_MS, deadline - this.now()));

    if (!this.areGone(identities)) {
      try {
        this.forceStop(parsed.name, identities);
      } catch {
        // The final process-identity check below decides whether the seat is gone or retained.
      }
    }
    const holderState = this.processState(identities.holder);
    const agentState = this.processState(identities.agent);
    if (holderState === "gone" && agentState === "gone")
      return { outcome: "reaped", detail: `seat ${parsed.name} process identities are gone after stop` };
    const detail = `wave-seat-${parsed.name} still has a live or unreadable process identity after forced stop`;
    return {
      outcome: "retained",
      reason: holderState === "unknown" || agentState === "unknown" ? "unprovable" : "members-remain",
      detail,
    };
  }

  /** The agent's own `run-p<agentPid>-*.scope` unit, read from `/proc/<agentPid>/cgroup` while the agent pid is live
   *  (called from spawn, never at wait time). The omp scope is not listed in the seat's `ConsistsOf`, so the wait must
   *  name it. Undefined when the agent runs in no run-p scope (for example on a host without systemd-run) or the cgroup
   *  cannot be read; the seat's own members still gate the wait then. */
  private agentRunScope(agentPid: number): string | undefined {
    let cgroup: string;
    try {
      cgroup = this.fs.readFileSync(`/proc/${agentPid}/cgroup`, "utf8");
    } catch {
      return undefined;
    }
    const path = cgroup.split("\n").find((line) => line.startsWith("0::"))?.slice(3);
    const scope = path?.split("/").at(-1);
    return scope !== undefined && /^run-p\d+-.*\.scope$/u.test(scope) ? path : undefined;
  }

  private readProcess(pid: number): ProcessRecord | undefined {
    if (this.platform === "darwin") {
      const token = this.darwinStartToken(pid);
      return token === undefined ? undefined : { pid, state: "S", startTime: token };
    }
    try {
      return parseProcessStat(this.fs.readFileSync(`/proc/${pid}/stat`, "utf8"), pid);
    } catch {
      return undefined;
    }
  }

  /** The start token wave-host reports for a live pid; undefined when absent or unreadable. */
  private darwinStartToken(pid: number): string | undefined {
    try {
      const out = this.exec(this.requireWaveHost(), ["process-identity", "--pid", String(pid)], { encoding: "utf8", timeout: START_TIMEOUT_MS });
      return /^\d+\s*$/u.test(out) ? out.trim() : undefined;
    } catch {
      return undefined;
    }
  }

  private requireWaveHost(): string {
    const command = this.waveHost();
    if (command === undefined) throw new SpawnRefused("holder runtime: WAVE_HOST_COMMAND is not set");
    return command;
  }

  private processState(identity: ProcessIdentity): ProcessState {
    if (this.platform === "darwin") {
      let token: string;
      try {
        token = this.exec(this.requireWaveHost(), ["process-identity", "--pid", String(identity.pid)], { encoding: "utf8", timeout: START_TIMEOUT_MS }).trim();
      } catch (error) {
        return exitStatus(error) === 3 ? "gone" : "unknown";
      }
      if (!/^\d+$/u.test(token)) return "unknown";
      return token === identity.startTime ? "alive" : "gone";
    }
    let raw: string;
    try {
      raw = this.fs.readFileSync(`/proc/${identity.pid}/stat`, "utf8");
    } catch (error) {
      return isMissingProcess(error) ? "gone" : "unknown";
    }
    const current = parseProcessStat(raw, identity.pid);
    if (current === undefined) return "unknown";
    if (current.startTime !== identity.startTime || current.state === "Z" || current.state === "X") return "gone";
    return "alive";
  }

  private status(identities: { holder: ProcessIdentity; agent: ProcessIdentity }): "running" | "exited" {
    return this.areGone(identities) ? "exited" : "running";
  }

  private areGone(identities: { holder: ProcessIdentity; agent: ProcessIdentity }): boolean {
    return this.processState(identities.holder) === "gone" && this.processState(identities.agent) === "gone";
  }

  private stopUnit(name: string, identities: { holder: ProcessIdentity; agent: ProcessIdentity }): void {
    if (this.areGone(identities)) return;
    try {
      this.stopSeatJob(name);
    } catch (error) {
      if (!this.areGone(identities)) throw error;
    }
  }

  /** Resolve once the seat's work has ended. Proof: the seat unit is `inactive` or `failed`, every unit it lists in
   *  `ConsistsOf` (its tmux-spawn scopes) is `inactive`, `failed` or absent, and each of those cgroups holds no process
   *  (`populated 0`). The agent's own run-p scope is waited on too, because the seat does not list it. The seat unit must
   *  also be unloaded (`LoadState=not-found`), so the restart's `start` is not refused by `unitLoaded`. A failed unit is
   *  reset until systemd unloads it. A live or transitioning unit keeps polling. An unknown state, a failed `systemctl`,
   *  or an unreadable cgroup rejects. The wait is capped at WORK_EXIT_MS and then rejects with what it last saw. */
  private async waitForUnitWorkExit(name: string, agentScope?: string): Promise<void> {
    const unit = `${this.prefix()}${name}`;
    const deadline = this.now() + WORK_EXIT_MS;
    for (;;) {
      const seat = parseUnitShow(this.exec("systemctl", [...UNIT_SHOW_ARGS, unit], { encoding: "utf8", timeout: START_TIMEOUT_MS }), unit);
      const agentEnded = agentScope === undefined || this.cgroupEmpty(agentScope);
      const busy = seat.LoadState === "not-found" ? undefined : this.busyUnit(seat, unit);
      if (busy === undefined && agentEnded) {
        if (seat.LoadState === "not-found") return;
        if (seat.ActiveState === "failed") this.resetFailed(unit);
      }
      if (this.now() >= deadline) {
        const observed = busy ?? (agentEnded ? `ActiveState=${seat.ActiveState}, still loaded` : `agent scope ${agentScope} populated`);
        throw new Error(`holder runtime: unit ${unit} not proved ended within ${WORK_EXIT_MS}ms (${observed})`);
      }
      await this.sleep(Math.min(POLL_MS, deadline - this.now()));
    }
  }

  /** The first member of the seat that still runs work, or undefined when every member has ended. A seat that is not
   *  inactive or failed is busy. An unknown active state rejects. */
  private busyUnit(seat: UnitShow, unit: string): string | undefined {
    if (seat.ActiveState !== "inactive" && seat.ActiveState !== "failed") {
      if (LIVE_UNIT_STATES[seat.ActiveState] !== true)
        throw new Error(`holder runtime: unit ${unit} is in unknown state ${seat.ActiveState}`);
      return `ActiveState=${seat.ActiveState}`;
    }
    if (!this.cgroupEmpty(seat.ControlGroup)) return `${unit} cgroup populated`;
    for (const member of seat.ConsistsOf.split(/\s+/u).filter((u) => u !== "")) {
      if (!this.memberEnded(member)) return `${member} populated`;
    }
    return undefined;
  }

  /** True when a listed member has ended: its state is inactive, failed or absent, and its cgroup holds no process. */
  private memberEnded(member: string): boolean {
    const state = parseUnitShow(this.exec("systemctl", [...MEMBER_SHOW_ARGS, member], { encoding: "utf8", timeout: START_TIMEOUT_MS }), member);
    if (state.LoadState === "not-found") return true;
    if (state.ActiveState !== "inactive" && state.ActiveState !== "failed") return false;
    return this.cgroupEmpty(state.ControlGroup);
  }

  /** Reset a proven `failed` unit. systemd garbage-collects a `--collect` unit at once, so a reset that finds the unit
   *  already `not loaded` has done its job. Any other failure still rejects. */
  private resetFailed(unit: string): void {
    try {
      this.exec("systemctl", ["--user", "reset-failed", unit], { encoding: "utf8" });
    } catch (error) {
      if (error instanceof Error && /not loaded/u.test(error.message)) return;
      throw error;
    }
  }

  /** True when the cgroup holds no process, read from `cgroup.events` so child cgroups count. A missing cgroup is
   *  empty; any other read error, or a file without a `populated` field, throws so the caller never resolves on it. */
  private cgroupEmpty(controlGroup: string): boolean {
    if (controlGroup === "") return true;
    const path = `/sys/fs/cgroup${controlGroup}/cgroup.events`;
    let events: string;
    try {
      events = this.fs.readFileSync(path, "utf8");
    } catch (error) {
      if (isMissingProcess(error)) return true;
      throw error;
    }
    const populated = /^populated (\d+)$/mu.exec(events)?.[1];
    if (populated === undefined) throw new Error(`holder runtime: no populated field in ${path}`);
    return populated === "0";
  }

  /** The graceful, platform-correct stop of a seat's job. Darwin removes its launchd job; Linux stops the unit. */
  private stopSeatJob(name: string): void {
    if (this.platform === "darwin") {
      this.exec(this.requireWaveHost(), ["seat-stop", "--name", name], { encoding: "utf8", timeout: START_TIMEOUT_MS });
      return;
    }
    this.exec("systemctl", ["--user", "stop", "--no-block", `${this.prefix()}${name}`], { encoding: "utf8" });
  }

  private forceStop(name: string, identities: { holder: ProcessIdentity; agent: ProcessIdentity }): void {
    if (this.areGone(identities)) return;
    try {
      if (this.processState(identities.agent) === "alive") this.kill(identities.agent.pid, "SIGKILL");
    } catch (error) {
      if (!isMissingProcess(error) && !this.areGone(identities)) throw error;
    }
    try {
      if (this.platform === "darwin") this.stopSeatJob(name);
      else this.exec("systemctl", ["--user", "kill", "-s", "SIGKILL", `${this.prefix()}${name}`], { encoding: "utf8" });
    } catch (error) {
      if (!this.areGone(identities)) throw error;
    }
  }
}

export const holderRuntimeProvider: RuntimeProvider = {
  kind: "runtime",
  name: "holder",
  available: () => process.env.COTAL_HOLDER_COMMAND !== undefined,
  create: () => new HolderRuntime(),
};

registry.register(holderRuntimeProvider);
