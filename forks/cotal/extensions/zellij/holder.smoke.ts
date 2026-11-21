import assert from "node:assert/strict";
import { existsSync, rmSync } from "node:fs";
import type { LaunchSpec } from "@cotal-ai/core";
import type { RuntimeReapEvidence as ManagerRuntimeReapEvidence } from "../../implementations/manager/src/runtime/index.js";
import { isCustodialRuntime } from "../../implementations/manager/src/runtime/index.js";
import { HolderRuntime, holderRuntimeProvider } from "./src/holder.js";

const reapEvidenceTypeCheck = {
  absent: { outcome: "absent" },
  reaped: { outcome: "reaped", detail: "test" },
  retained: { outcome: "retained", reason: "unprovable", detail: "test" },
} satisfies Record<string, ManagerRuntimeReapEvidence>;

type ExecOptions = { cwd?: string; encoding: "utf8"; timeout?: number };
type ExecCall = { file: string; args: string[]; cwd?: string; timeout?: number };
type FixtureOptions = {
  readonly startError?: Error;
  /** The agent's own cgroup path, as `/proc/<agentPid>/cgroup` reports it. The holder finds its run-p scope here. */
  readonly agentCgroup?: string;
  readonly startOutput?: string;
  readonly systemctlError?: Error;
  readonly onExec?: (file: string, args: string[], proc: Map<number, string>) => void;
  readonly onSleep?: (ms: number, proc: Map<number, string>) => void;
  /** Fake `systemctl show` answer for the seat unit; absent means the unit is gone. */
  readonly unit?: () => Partial<UnitState> | undefined;
  /** Units the seat lists as `ConsistsOf`, e.g. its tmux-spawn scope. */
  readonly consistsOf?: string[];
  /** Fake `systemctl show` answers for member units, keyed by unit name. An absent key means the unit is gone. */
  readonly members?: Record<string, () => Partial<UnitState> | undefined>;
  /** Raw `systemctl show` output, to exercise a parse failure. Wins over `unit`. */
  readonly unitShowRaw?: string;
  /** Fake `cgroup.events` contents keyed by cgroup path. A path not listed reads as gone. */
  readonly cgroupEvents?: (path: string) => string | undefined;
  readonly cgroupEventsError?: Error;
  readonly resetFailedError?: Error;
  readonly unitShowError?: Error;
};

type UnitState = { LoadState: string; ActiveState: string; SubState: string; Result: string; ControlGroup: string; ConsistsOf: string };

const holder = "/fake/wave-seat-holder";
const seatName = "ztest-holder";
const launchSpec: LaunchSpec = { command: "node", args: ["agent"] };
let checks = 0;
const seatShowArgs = ["--user", "show", "-p", "LoadState", "-p", "ActiveState", "-p", "ControlGroup", "-p", "ConsistsOf", `wave-seat-${seatName}`];
const memberShowArgs = ["--user", "show", "-p", "LoadState", "-p", "ActiveState", "-p", "ControlGroup"];

function stat(pid: number, startTime: number): string {
  const fields = Array.from({ length: 19 }, (_, index) => String(index + 1));
  fields[18] = String(startTime);
  return `${pid} (fake seat process) S ${fields.join(" ")}\n`;
}

function fixture(options: FixtureOptions = {}) {
  const proc = new Map<number, string>([[101, stat(101, 11)], [202, stat(202, 22)]]);
  const calls: ExecCall[] = [];
  const sleeps: number[] = [];
  const kills: Array<{ pid: number; signal: string }> = [];
  const launcherDirs: string[] = [];
  let clock = 0;
  const runtime = new HolderRuntime({
    platform: "linux",
    holderCommand: () => holder,
    unitPrefix: () => "wave-seat-",
    execFileSync: (file: string, args: string[], execOptions: ExecOptions) => {
      if (file === holder && args[0] === "start" && execOptions.cwd !== undefined) launcherDirs.push(execOptions.cwd);
      calls.push({
        file,
        args,
        ...(execOptions.cwd === undefined ? {} : { cwd: execOptions.cwd }),
        ...(execOptions.timeout === undefined ? {} : { timeout: execOptions.timeout }),
      });
      options.onExec?.(file, args, proc);
      if (file === holder && args[0] === "start") {
        if (options.startError !== undefined) throw options.startError;
        return options.startOutput ?? "101 202\n";
      }
      if (file === "systemctl" && options.unitShowError !== undefined && args[1] === "show") throw options.unitShowError;
      if (file === "systemctl" && args[1] === "show") {
        // Only the seat and its listed members are answered. Any other unit or property list fails the check that made the call.
        const unitName = args[args.length - 1] ?? "";
        if (unitName === `wave-seat-${seatName}`) {
          assert.deepEqual(args, seatShowArgs, "waitForWorkExit must read the seat unit state only");
          return options.unitShowRaw ?? unitShow(options.unit?.(), options.consistsOf ?? []);
        }
        assert.deepEqual(args, [...memberShowArgs, unitName], `unexpected systemctl show for ${unitName}`);
        const member = options.members?.[unitName];
        assert.ok(member !== undefined, `no fake member for ${unitName}`);
        return memberShow(member());
      }
      if (file === "systemctl" && args[1] === "reset-failed" && options.resetFailedError !== undefined) throw options.resetFailedError;
      if (file === "systemctl" && options.systemctlError !== undefined) throw options.systemctlError;
      return "";
    },
    fs: {
      readFileSync: (path: string) => {
        if (/\/cgroup\.procs$/.test(path)) throw new Error("holder must read cgroup.events, not cgroup.procs");
        if (/\/cgroup\.events$/.test(path)) {
          if (options.cgroupEventsError !== undefined) throw options.cgroupEventsError;
          const cgroupDir = path.slice("/sys/fs/cgroup".length, -"/cgroup.events".length);
          const events = options.cgroupEvents?.(cgroupDir);
          if (events === undefined) throw Object.assign(new Error("not found"), { code: "ENOENT" });
          return events;
        }
        const cgroupOf = /^\/proc\/(\d+)\/cgroup$/.exec(path);
        if (cgroupOf !== null) {
          if (options.agentCgroup === undefined) throw Object.assign(new Error("not found"), { code: "ENOENT" });
          return `0::${options.agentCgroup}\n`;
        }
        const match = /^\/proc\/(\d+)\/stat$/.exec(path);
        const pid = match?.[1] === undefined ? undefined : Number(match[1]);
        const content = pid === undefined ? undefined : proc.get(pid);
        if (content === undefined) throw Object.assign(new Error("not found"), { code: "ENOENT" });
        return content;
      },
    },
    sleep: async (ms: number) => {
      sleeps.push(ms);
      options.onSleep?.(ms, proc);
      clock += ms;
    },
    now: () => clock,
    kill: (pid: number, signal: NodeJS.Signals) => kills.push({ pid, signal }),
  });

  const spawn = (spec = launchSpec) => runtime.spawn(seatName, spec, process.cwd());
  const cleanup = () => {
    for (const dir of launcherDirs) rmSync(dir, { recursive: true, force: true });
  };
  return { runtime, proc, calls, sleeps, kills, spawn, cleanup };
}

function unitShow(state: Partial<UnitState> | undefined, consistsOf: string[] = []): string {
  const full: UnitState = state === undefined
    ? { LoadState: "not-found", ActiveState: "inactive", SubState: "dead", Result: "success", ControlGroup: "", ConsistsOf: "" }
    : { LoadState: "loaded", ActiveState: "inactive", SubState: "dead", Result: "success", ControlGroup: "", ConsistsOf: consistsOf.join(" "), ...state };
  return `LoadState=${full.LoadState}\nActiveState=${full.ActiveState}\nSubState=${full.SubState}\nResult=${full.Result}\nControlGroup=${full.ControlGroup}\nConsistsOf=${full.ConsistsOf}\n`;
}

/** `systemctl show -p LoadState -p ActiveState -p ControlGroup <unit>`. An absent member answers as not-found. */
function memberShow(state: Partial<UnitState> | undefined): string {
  const full: UnitState = state === undefined
    ? { LoadState: "not-found", ActiveState: "inactive", SubState: "dead", Result: "success", ControlGroup: "", ConsistsOf: "" }
    : { LoadState: "loaded", ActiveState: "inactive", SubState: "dead", Result: "success", ControlGroup: "/seat", ConsistsOf: "", ...state };
  return `LoadState=${full.LoadState}\nActiveState=${full.ActiveState}\nControlGroup=${full.ControlGroup}\n`;
}

async function check(name: string, run: () => void | Promise<void>): Promise<void> {
  await run();
  checks++;
  console.log(`  ✓ ${name}`);
}

await check("holder status is live without Zellij on PATH", () => {
  const previousPath = process.env.PATH;
  process.env.PATH = "";
  const f = fixture();
  try {
    const handle = f.spawn();
    assert.equal(handle.status(), "running");
    assert.deepEqual(f.calls.map((call) => call.file), [holder]);
  } finally {
    f.cleanup();
    if (previousPath === undefined) delete process.env.PATH;
    else process.env.PATH = previousPath;
  }
});

await check("spawn passes the private launcher to holder and records both process identities", () => {
  const f = fixture();
  try {
    const handle = f.spawn();
    assert.equal(handle.kind, "holder");
    assert.equal(handle.pid, 202);
    assert.deepEqual(handle.reference, { kind: "holder", id: `${seatName}:101:11:202:22` });
    const call = f.calls[0];
    assert.deepEqual(call, {
      file: holder,
      args: ["start", "--name", seatName, "--", "./ztest-holder.cjs"],
      cwd: call?.cwd,
      timeout: 15_000,
    });
    assert.ok(call?.cwd);
  } finally {
    f.cleanup();
  }
});

await check("startup confirmation is refused before calling the holder", () => {
  const f = fixture();
  try {
    assert.throws(() => f.spawn({ ...launchSpec, confirm: "continue?" }), (error: unknown) =>
      error instanceof Error && error.name === "SpawnRefused",
    );
    assert.equal(f.calls.length, 0);
  } finally {
    f.cleanup();
  }
});

await check("status exits only after both process identities are gone", () => {
  const f = fixture();
  try {
    const handle = f.spawn();
    f.proc.delete(101);
    assert.equal(handle.status(), "running");
    f.proc.delete(202);
    assert.equal(handle.status(), "exited");
  } finally {
    f.cleanup();
  }
});

await check("a starttime mismatch counts as a dead process identity", () => {
  const f = fixture();
  try {
    const handle = f.spawn();
    f.proc.set(202, stat(202, 23));
    assert.equal(handle.status(), "running");
    f.proc.delete(101);
    assert.equal(handle.status(), "exited");
  } finally {
    f.cleanup();
  }
});

await check("a dead reference reaps with proof", async () => {
  const f = fixture();
  try {
    const handle = f.spawn();
    f.proc.clear();
    const reference = handle.reference;
    assert.ok(reference);
    assert.equal((await f.runtime.reap(reference)).outcome, "reaped");
    assert.equal(f.calls.length, 1);
  } finally {
    f.cleanup();
  }
});

await check("a malformed reference is absent", async () => {
  const f = fixture();
  try {
    assert.deepEqual(await f.runtime.reap({ kind: "holder", id: "not-a-seat" }), { outcome: "absent" });
    assert.equal(f.calls.length, 0);
  } finally {
    f.cleanup();
  }
});

await check("holder exit 4 becomes the actionable SpawnRefused", () => {
  const f = fixture({ startError: Object.assign(new Error("holder exited 4"), { status: 4 }) });
  try {
    assert.throws(() => f.spawn(), (error: unknown) =>
      error instanceof Error && error.name === "SpawnRefused" &&
      error.message === `seat ${seatName} is already live; stop it with: systemctl --user stop wave-seat-${seatName}`,
    );
  } finally {
    f.cleanup();
  }
});

await check("holder runtime is not custodial", () => {
  const f = fixture();
  try {
    assert.equal(isCustodialRuntime(f.runtime), false);
  } finally {
    f.cleanup();
  }
});

await check("graceful stop does not wait for the unit to exit", () => {
  const f = fixture();
  try {
    const handle = f.spawn();
    handle.stop({ graceful: true });
    assert.deepEqual(f.calls.at(-1), {
      file: "systemctl",
      args: ["--user", "stop", "--no-block", `wave-seat-${seatName}`],
    });
  } finally {
    f.cleanup();
  }
});

await check("graceful and forced stop use the seat unit and verified agent pid", () => {
  const f = fixture();
  try {
    const handle = f.spawn();
    handle.stop({ graceful: true });
    assert.deepEqual(f.calls.at(-1), {
      file: "systemctl",
      args: ["--user", "stop", "--no-block", `wave-seat-${seatName}`],
    });
    handle.stop({ graceful: false });
    assert.deepEqual(f.kills, [{ pid: 202, signal: "SIGKILL" }]);
    assert.deepEqual(f.calls.at(-1), {
      file: "systemctl",
      args: ["--user", "kill", "-s", "SIGKILL", `wave-seat-${seatName}`],
    });
  } finally {
    f.cleanup();
  }
});

await check("forced stop never SIGKILLs a reused agent pid whose starttime changed", () => {
  const f = fixture();
  try {
    const handle = f.spawn();
    // The holder stays alive, but pid 202 now belongs to another process.
    f.proc.set(202, stat(202, 23));
    handle.stop({ graceful: false });
    assert.deepEqual(f.kills, []);
  } finally {
    f.cleanup();
  }
});

await check("a post-start parse failure stops the unit and removes the launcher", () => {
  const f = fixture({ startOutput: "not a pid pair" });
  try {
    assert.throws(() => f.spawn(), /invalid start output/);
    assert.deepEqual(f.calls.at(-1), {
      file: "systemctl",
      args: ["--user", "stop", "--no-block", `wave-seat-${seatName}`],
    });
    const launcherDir = f.calls[0]?.cwd;
    assert.ok(launcherDir);
    assert.equal(existsSync(launcherDir), false);
  } finally {
    f.cleanup();
  }
});

await check("a post-start /proc identity failure also stops the accepted unit", () => {
  const f = fixture({
    onExec: (file, args, proc) => {
      if (file === holder && args[0] === "start") proc.delete(202);
    },
  });
  try {
    assert.throws(() => f.spawn(), /could not read live process identities/);
    assert.deepEqual(f.calls.at(-1), {
      file: "systemctl",
      args: ["--user", "stop", "--no-block", `wave-seat-${seatName}`],
    });
    const launcherDir = f.calls[0]?.cwd;
    assert.ok(launcherDir);
    assert.equal(existsSync(launcherDir), false);
  } finally {
    f.cleanup();
  }
});

await check("stop on an exited seat does not throw or call systemctl", () => {
  const f = fixture({ systemctlError: new Error("unit no longer loaded") });
  try {
    const handle = f.spawn();
    f.proc.clear();
    assert.doesNotThrow(() => handle.stop({ graceful: true }));
    assert.doesNotThrow(() => handle.stop({ graceful: false }));
    assert.equal(f.calls.filter((call) => call.file === "systemctl").length, 0);
  } finally {
    f.cleanup();
  }
});

await check("systemctl failure is swallowed only after both identities exit", () => {
  const f = fixture({
    systemctlError: new Error("unit disappeared"),
    onExec: (file, args, proc) => {
      if (file === "systemctl" && args[1] === "stop") proc.clear();
    },
  });
  try {
    const handle = f.spawn();
    assert.doesNotThrow(() => handle.stop({ graceful: true }));
  } finally {
    f.cleanup();
  }
});

await check("systemctl failure is rethrown while a process identity survives", () => {
  const failure = new Error("systemctl stop failed");
  const f = fixture({ systemctlError: failure });
  try {
    const handle = f.spawn();
    assert.throws(() => handle.stop({ graceful: true }), failure);
  } finally {
    f.cleanup();
  }
});

await check("interrupt is bounded and attach uses the native holder command", () => {
  const f = fixture();
  try {
    const handle = f.spawn();
    handle.interrupt();
    assert.deepEqual(f.calls.at(-1), { file: holder, args: ["interrupt", "--name", seatName], timeout: 15_000 });
    assert.throws(() => handle.attach(), /holder runtime: attach with wave-seat-holder attach/);
  } finally {
    f.cleanup();
  }
});

await check("waitForExit checks again after each one-second poll", async () => {
  let polls = 0;
  const f = fixture({ onSleep: (_ms, proc) => { polls++; if (polls === 1) proc.clear(); } });
  try {
    const handle = f.spawn();
    await handle.waitForExit?.();
    assert.deepEqual(f.sleeps, [1_000]);
    assert.equal(handle.status(), "exited");
  } finally {
    f.cleanup();
  }
});

await check("linux handle supports work closure", () => {
  assert.equal(new HolderRuntime({ platform: "linux" }).supportsWorkClosure, true);
});

const seatCgroup = "/seat";
const toolCgroup = "/tmux-spawn-test.scope";
const agentCgroup = "/run-p101-i1.scope";
const toolMember = "tmux-spawn-test.scope";
const agentMember = "run-p101-i1.scope";

/** The real layout: the seat's own cgroup is empty and its work is in the tmux-spawn member scope. */
function seatLayout(events: Record<string, string | undefined> = {}, seat: Partial<UnitState> = {}): Partial<FixtureOptions> {
  return {
    unit: () => ({ ActiveState: "inactive", SubState: "dead", ControlGroup: "", ...seat }),
    consistsOf: [toolMember],
    members: { [toolMember]: () => ({ ControlGroup: toolCgroup, ActiveState: "inactive" }) },
    cgroupEvents: (path: string) => ({ [toolCgroup]: "populated 0\n", [seatCgroup]: "populated 0\n", ...events })[path],
  };
}

await check("waitForWorkExit resolves for an inactive seat whose members and cgroups are empty, once systemd unloads it", async () => {
  let polls = 0;
  let unit: Partial<UnitState> | undefined = { ActiveState: "inactive", SubState: "dead", ControlGroup: "" };
  const f = fixture({
    ...seatLayout(),
    unit: () => unit,
    onSleep: () => {
      polls++;
      if (polls === 1) unit = undefined;
    },
  });
  try {
    const handle = f.spawn();
    f.proc.clear();
    assert.ok(handle.waitForWorkExit, "linux handle must expose waitForWorkExit");
    await handle.waitForWorkExit();
    assert.equal(polls, 1, "the wait must hold one poll on the loaded inactive seat, then resolve on unload");
    assert.deepEqual(f.calls.filter((call) => call.args[1] === "show").map((call) => call.args), [seatShowArgs, [...memberShowArgs, toolMember], seatShowArgs]);
    assert.equal(f.calls.some((call) => call.args[1] === "reset-failed"), false);
  } finally {
    f.cleanup();
  }
});

await check("waitForWorkExit keeps polling while a seat member scope still holds a process", async () => {
  let polls = 0;
  let events = "populated 1\n";
  let unit: Partial<UnitState> | undefined = { ActiveState: "inactive", SubState: "dead", ControlGroup: "" };
  const f = fixture({
    ...seatLayout(),
    unit: () => unit,
    cgroupEvents: (path: string) => (path === toolCgroup ? events : "populated 0\n"),
    onSleep: () => {
      polls++;
      if (polls === 2) events = "populated 0\n";
      if (polls === 3) unit = undefined;
    },
  });
  try {
    const handle = f.spawn();
    f.proc.clear();
    assert.ok(handle.waitForWorkExit, "linux handle must expose waitForWorkExit");
    await handle.waitForWorkExit();
    assert.ok(polls >= 2, `must wait for the tmux-spawn scope to empty, polled ${polls}`);
  } finally {
    f.cleanup();
  }
});

await check("waitForWorkExit does not resolve on an empty seat cgroup while its member is still active", async () => {
  let polls = 0;
  let member: Partial<UnitState> = { ActiveState: "active", ControlGroup: toolCgroup };
  let unit: Partial<UnitState> | undefined = { ActiveState: "inactive", SubState: "dead", ControlGroup: "" };
  const f = fixture({
    unit: () => unit,
    consistsOf: [toolMember],
    members: { [toolMember]: () => member },
    cgroupEvents: () => "populated 0\n",
    onSleep: () => {
      polls++;
      if (polls === 2) member = { ActiveState: "inactive", ControlGroup: toolCgroup };
      if (polls === 3) unit = undefined;
    },
  });
  try {
    const handle = f.spawn();
    f.proc.clear();
    assert.ok(handle.waitForWorkExit, "linux handle must expose waitForWorkExit");
    await handle.waitForWorkExit();
    assert.ok(polls >= 2, `the active member must hold the wait, polled ${polls}`);
  } finally {
    f.cleanup();
  }
});

await check("waitForWorkExit holds the wait until the seat unit is unloaded, so the restart start is accepted", async () => {
  let polls = 0;
  let loaded = "loaded";
  const f = fixture({
    ...seatLayout(),
    unit: () => ({ LoadState: loaded, ActiveState: "inactive", SubState: "dead", ControlGroup: "" }),
    onSleep: () => { polls++; if (polls === 2) loaded = "not-found"; },
  });
  try {
    const handle = f.spawn();
    f.proc.clear();
    assert.ok(handle.waitForWorkExit, "linux handle must expose waitForWorkExit");
    await handle.waitForWorkExit();
    assert.ok(polls >= 2, `must poll until the unit unloads, polled ${polls}`);
  } finally {
    f.cleanup();
  }
});

await check("waitForWorkExit resets a failed oom-kill seat, then holds until systemd unloads it", async () => {
  let unit: Partial<UnitState> | undefined = { ActiveState: "failed", SubState: "failed", Result: "oom-kill", ControlGroup: "" };
  const f = fixture({
    ...seatLayout({}, { ActiveState: "failed", SubState: "failed", Result: "oom-kill" }),
    unit: () => unit,
    onExec: (file, args) => { if (file === "systemctl" && args[1] === "reset-failed") unit = undefined; },
  });
  try {
    const handle = f.spawn();
    f.proc.clear();
    assert.ok(handle.waitForWorkExit, "linux handle must expose waitForWorkExit");
    await handle.waitForWorkExit();
    assert.deepEqual(f.calls.filter((call) => call.args[1] === "reset-failed"), [{ file: "systemctl", args: ["--user", "reset-failed", `wave-seat-${seatName}`] }]);
  } finally {
    f.cleanup();
  }
});

await check("waitForWorkExit resolves when reset-failed reports the unit not loaded", async () => {
  let collected = false;
  const f = fixture({
    ...seatLayout({}, { ActiveState: "failed", SubState: "failed", Result: "oom-kill" }),
    unit: () => (collected ? undefined : { ActiveState: "failed", SubState: "failed", Result: "oom-kill", ControlGroup: "" }),
    onExec: (file, args) => { if (file === "systemctl" && args[1] === "reset-failed") collected = true; },
    resetFailedError: new Error(`Command failed: systemctl --user reset-failed wave-seat-${seatName}\nUnit wave-seat-${seatName}.service not loaded.`),
  });
  try {
    const handle = f.spawn();
    f.proc.clear();
    assert.ok(handle.waitForWorkExit, "linux handle must expose waitForWorkExit");
    await handle.waitForWorkExit();
    assert.equal(f.calls.filter((call) => call.args[1] === "reset-failed").length, 1);
  } finally {
    f.cleanup();
  }
});

await check("waitForWorkExit rejects when reset-failed fails for any other reason", async () => {
  const f = fixture({
    unit: () => ({ ActiveState: "failed", SubState: "failed", Result: "oom-kill", ControlGroup: "" }),
    resetFailedError: new Error("Failed to connect to bus: No medium found"),
  });
  try {
    const handle = f.spawn();
    f.proc.clear();
    assert.ok(handle.waitForWorkExit, "linux handle must expose waitForWorkExit");
    await assert.rejects(handle.waitForWorkExit(), /Failed to connect to bus/);
  } finally {
    f.cleanup();
  }
});

await check("waitForWorkExit keeps polling while the unit is active and resolves only after unit and cgroups end", async () => {
  let polls = 0;
  let unit: Partial<UnitState> | undefined = { ActiveState: "active", SubState: "running", ControlGroup: "" };
  let events = "populated 1\n";
  const f = fixture({
    unit: () => unit,
    consistsOf: [toolMember],
    members: { [toolMember]: () => ({ ControlGroup: toolCgroup, ActiveState: "inactive" }) },
    cgroupEvents: (path: string) => (path === toolCgroup ? events : "populated 0\n"),
    onSleep: () => {
      polls++;
      if (polls === 2) unit = { ActiveState: "inactive", SubState: "dead", ControlGroup: "" };
      if (polls === 3) events = "populated 0\n";
      if (polls === 4) unit = undefined;
    },
  });
  try {
    const handle = f.spawn();
    f.proc.clear();
    assert.ok(handle.waitForWorkExit, "linux handle must expose waitForWorkExit");
    await handle.waitForWorkExit();
    assert.ok(polls >= 4, `must poll through active, survivor and loaded-inactive states, polled ${polls}`);
  } finally {
    f.cleanup();
  }
});

await check("waitForWorkExit keeps polling while the unit is deactivating", async () => {
  let unit: Partial<UnitState> | undefined = { ActiveState: "deactivating", SubState: "stop-sigterm", ControlGroup: "" };
  const f = fixture({
    unit: () => unit,
    onSleep: () => {
      if (f.sleeps.length === 2) unit = { ActiveState: "inactive", SubState: "dead", ControlGroup: "" };
      if (f.sleeps.length === 3) unit = undefined;
    },
  });
  try {
    const handle = f.spawn();
    f.proc.clear();
    assert.ok(handle.waitForWorkExit, "linux handle must expose waitForWorkExit");
    await handle.waitForWorkExit();
    assert.deepEqual(f.sleeps, [1_000, 1_000, 1_000]);
  } finally {
    f.cleanup();
  }
});

await check("waitForWorkExit keeps polling while the unit is activating", async () => {
  let unit: Partial<UnitState> | undefined = { ActiveState: "activating", SubState: "start", ControlGroup: "" };
  const f = fixture({
    unit: () => unit,
    onSleep: () => {
      if (f.sleeps.length === 2) unit = { ActiveState: "inactive", SubState: "dead", ControlGroup: "" };
      if (f.sleeps.length === 3) unit = undefined;
    },
  });
  try {
    const handle = f.spawn();
    f.proc.clear();
    assert.ok(handle.waitForWorkExit, "linux handle must expose waitForWorkExit");
    await handle.waitForWorkExit();
    assert.deepEqual(f.sleeps, [1_000, 1_000, 1_000]);
  } finally {
    f.cleanup();
  }
});

await check("waitForWorkExit keeps polling while the unit is reloading", async () => {
  let unit: Partial<UnitState> | undefined = { ActiveState: "reloading", SubState: "reload", ControlGroup: "" };
  const f = fixture({
    unit: () => unit,
    onSleep: () => {
      if (f.sleeps.length === 2) unit = { ActiveState: "inactive", SubState: "dead", ControlGroup: "" };
      if (f.sleeps.length === 3) unit = undefined;
    },
  });
  try {
    const handle = f.spawn();
    f.proc.clear();
    assert.ok(handle.waitForWorkExit, "linux handle must expose waitForWorkExit");
    await handle.waitForWorkExit();
    assert.deepEqual(f.sleeps, [1_000, 1_000, 1_000]);
  } finally {
    f.cleanup();
  }
});

await check("waitForWorkExit rejects on an unknown unit state", async () => {
  const f = fixture({ unit: () => ({ ActiveState: "maintenance", SubState: "maintenance", ControlGroup: "" }) });
  try {
    const handle = f.spawn();
    f.proc.clear();
    assert.ok(handle.waitForWorkExit, "linux handle must expose waitForWorkExit");
    await assert.rejects(handle.waitForWorkExit(), /unknown state maintenance/);
  } finally {
    f.cleanup();
  }
});

await check("waitForWorkExit resolves for an absent unit without reading any cgroup", async () => {
  const f = fixture({ unit: () => undefined, cgroupEvents: () => "populated 1\n" });
  try {
    const handle = f.spawn();
    f.proc.clear();
    assert.ok(handle.waitForWorkExit, "linux handle must expose waitForWorkExit");
    await handle.waitForWorkExit();
    assert.equal(f.calls.some((call) => call.args[1] === "reset-failed"), false);
    assert.deepEqual(f.sleeps, []);
  } finally {
    f.cleanup();
  }
});

await check("waitForWorkExit rejects when a member cgroup cannot be read", async () => {
  const f = fixture({
    ...seatLayout(),
    cgroupEventsError: Object.assign(new Error("permission denied"), { code: "EACCES" }),
  });
  try {
    const handle = f.spawn();
    f.proc.clear();
    assert.ok(handle.waitForWorkExit, "linux handle must expose waitForWorkExit");
    await assert.rejects(handle.waitForWorkExit(), /permission denied/);
  } finally {
    f.cleanup();
  }
});

await check("waitForWorkExit rejects when cgroup.events has no populated field", async () => {
  const f = fixture({ ...seatLayout({ [toolCgroup]: "frozen 0\n" }) });
  try {
    const handle = f.spawn();
    f.proc.clear();
    assert.ok(handle.waitForWorkExit, "linux handle must expose waitForWorkExit");
    await assert.rejects(handle.waitForWorkExit(), /no populated field/);
  } finally {
    f.cleanup();
  }
});

await check("waitForWorkExit rejects when the unit state cannot be parsed", async () => {
  const f = fixture({ unitShowRaw: "ActiveState=inactive\n" });
  try {
    const handle = f.spawn();
    f.proc.clear();
    assert.ok(handle.waitForWorkExit, "linux handle must expose waitForWorkExit");
    await assert.rejects(handle.waitForWorkExit(), /could not read unit state/);
  } finally {
    f.cleanup();
  }
});

await check("waitForWorkExit gives up after 9 seconds and names the last observed state", async () => {
  let polls = 0;
  const f = fixture({
    unit: () => ({ ActiveState: "active", SubState: "running", ControlGroup: "" }),
    onSleep: () => {
      polls++;
      if (polls > 50) throw new Error("poll loop never stopped");
    },
  });
  try {
    const handle = f.spawn();
    f.proc.clear();
    assert.ok(handle.waitForWorkExit, "linux handle must expose waitForWorkExit");
    await assert.rejects(handle.waitForWorkExit(), /not proved ended within 9000ms \(ActiveState=active\)/);
    assert.equal(f.sleeps.reduce((sum, ms) => sum + ms, 0), 9_000);
  } finally {
    f.cleanup();
  }
});

await check("waitForWorkExit gives up on a seat whose member cgroup stays populated and names it", async () => {
  let polls = 0;
  const f = fixture({
    ...seatLayout({ [toolCgroup]: "populated 1\n" }),
    onSleep: () => { polls++; if (polls > 50) throw new Error("poll loop never stopped"); },
  });
  try {
    const handle = f.spawn();
    f.proc.clear();
    assert.ok(handle.waitForWorkExit, "linux handle must expose waitForWorkExit");
    await assert.rejects(handle.waitForWorkExit(), new RegExp(`not proved ended within 9000ms \\(${toolMember} populated\\)`));
  } finally {
    f.cleanup();
  }
});

await check("waitForWorkExit holds a failed seat open while a detached survivor keeps a member cgroup", async () => {
  let polls = 0;
  let events = "populated 1\n";
  let unit: Partial<UnitState> | undefined = { ActiveState: "failed", SubState: "failed", Result: "exit-code", ControlGroup: "" };
  const f = fixture({
    ...seatLayout({}, { ActiveState: "failed", SubState: "failed", Result: "exit-code" }),
    unit: () => unit,
    cgroupEvents: (path: string) => (path === toolCgroup ? events : "populated 0\n"),
    onSleep: () => {
      polls++;
      if (polls === 2) events = "populated 0\n";
      if (polls === 3) unit = undefined;
    },
  });
  try {
    const handle = f.spawn();
    f.proc.clear();
    assert.ok(handle.waitForWorkExit, "linux handle must expose waitForWorkExit");
    await handle.waitForWorkExit();
    assert.ok(polls >= 3, `must wait for the survivor to leave its member cgroup, polled ${polls}`);
  } finally {
    f.cleanup();
  }
});

await check("waitForWorkExit rejects when systemctl cannot be read", async () => {
  const f = fixture({ unitShowError: new Error("systemctl: bus unavailable") });
  try {
    const handle = f.spawn();
    f.proc.clear();
    assert.ok(handle.waitForWorkExit, "linux handle must expose waitForWorkExit");
    await assert.rejects(handle.waitForWorkExit(), /bus unavailable/);
  } finally {
    f.cleanup();
  }
});

await check("waitForWorkExit holds the wait while the agent's run-p scope keeps its cgroup, found from /proc", async () => {
  let polls = 0;
  let events = "populated 1\n";
  let unit: Partial<UnitState> | undefined = { ActiveState: "inactive", SubState: "dead", ControlGroup: "" };
  const f = fixture({
    ...seatLayout(),
    unit: () => unit,
    agentCgroup,
    cgroupEvents: (path: string) => (path === agentCgroup ? events : "populated 0\n"),
    onSleep: () => {
      polls++;
      if (polls === 2) events = "populated 0\n";
      if (polls === 3) unit = undefined;
    },
  });
  try {
    const handle = f.spawn();
    f.proc.clear();
    assert.ok(handle.waitForWorkExit, "linux handle must expose waitForWorkExit");
    await handle.waitForWorkExit();
    assert.ok(polls >= 3, `the agent run-p scope must hold the wait, polled ${polls}`);
  } finally {
    f.cleanup();
  }
});

await check("waitForWorkExit keeps holding when the seat unloads while the agent's run-p scope still holds a process", async () => {
  let polls = 0;
  let events = "populated 1\n";
  const f = fixture({
    ...seatLayout(),
    unit: () => undefined,
    agentCgroup,
    cgroupEvents: (path: string) => (path === agentCgroup ? events : "populated 0\n"),
    onSleep: () => {
      polls++;
      if (polls === 2) events = "populated 0\n";
    },
  });
  try {
    const handle = f.spawn();
    f.proc.clear();
    assert.ok(handle.waitForWorkExit, "linux handle must expose waitForWorkExit");
    await handle.waitForWorkExit();
    assert.ok(polls >= 2, `the agent scope must hold the wait even after the seat unloads, polled ${polls}`);
  } finally {
    f.cleanup();
  }
});

await check("reap escalates after 30 seconds and reports a forced teardown", async () => {
  const f = fixture({ onExec: (file, args, proc) => { if (file === "systemctl" && args[1] === "kill") proc.clear(); } });
  try {
    const handle = f.spawn();
    const reference = handle.reference;
    assert.ok(reference);
    const evidence = await f.runtime.reap(reference);
    assert.equal(evidence.outcome, "reaped");
    assert.deepEqual(f.sleeps, Array.from({ length: 30 }, () => 1_000));
    assert.deepEqual(f.calls.slice(-2).map(({ args }) => args), [
      ["--user", "stop", "--no-block", `wave-seat-${seatName}`],
      ["--user", "kill", "-s", "SIGKILL", `wave-seat-${seatName}`],
    ]);
  } finally {
    f.cleanup();
  }
});

await check("reap reports retained when identities survive forced teardown", async () => {
  const f = fixture();
  try {
    const handle = f.spawn();
    const reference = handle.reference;
    assert.ok(reference);
    const evidence = await f.runtime.reap(reference);
    assert.equal(evidence.outcome, "retained");
    assert.deepEqual(evidence, {
      outcome: "retained",
      reason: "members-remain",
      detail: `wave-seat-${seatName} still has a live or unreadable process identity after forced stop`,
    });
  } finally {
    f.cleanup();
  }
});

await check("provider availability follows COTAL_HOLDER_COMMAND", () => {
  const previous = process.env.COTAL_HOLDER_COMMAND;
  try {
    delete process.env.COTAL_HOLDER_COMMAND;
    assert.equal(holderRuntimeProvider.available(), false);
    process.env.COTAL_HOLDER_COMMAND = holder;
    assert.equal(holderRuntimeProvider.available(), true);
  } finally {
    if (previous === undefined) delete process.env.COTAL_HOLDER_COMMAND;
    else process.env.COTAL_HOLDER_COMMAND = previous;
  }
});

console.log(`HOLDER PACKAGE TESTS: ${checks} tests executed`);
