/**
 * The pi extension's background-work wiring (extension.ts): the `background_work_changed` subscription,
 * the reads at session start, agent start, tool end and agent end, and the older-host guard. The
 * driver's collaborators are stubbed on the runtime cotalMesh registers, so each callback records only
 * what it hands the driver. The event path must re-read `ctx.getBackgroundWork()` and ignore its payload.
 * A host with the publisher is read at session start, so a seat with no work publishes `idle` at startup.
 * Run: tsx smoke/background-wiring.smoke.ts
 */
import assert from "node:assert/strict";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import type { PiDriver } from "../src/driver.js";
import cotalMesh from "../src/extension.js";

const RUNTIMES = Symbol.for("cotal.pi.runtimes");
const ONCE = Symbol.for("cotal.once-handoff");
const KEYS = [
  "COTAL_NAME", "COTAL_AGENT_FILE", "COTAL_LINK", "COTAL_HOME", "COTAL_DEFAULT_AGENT",
  "COTAL_CONTROL_SOCKET", "COTAL_CONTROL_TOKEN", "COTAL_LAUNCH_MATERIAL", "COTAL_ACCEPTED_TOKEN", "COTAL_CREDS",
  "COTAL_SPACE", "COTAL_SERVERS", "COTAL_SUBSCRIBE", "COTAL_ALLOW_PUBLISH", "COTAL_EVENTS", "COTAL_TOKEN",
  "COTAL_ID", "COTAL_LIFECYCLE_UID", "PI_SESSION_ID", "COTAL_PI_SESSION_STATE", "COTAL_PI_EXPECTED_SESSION",
  "COTAL_PI_FRESH_SESSION", "COTAL_PI_SUPERVISED_CWD", "COTAL_PI_PERSONA_FILE",
] as const;

type Counts = { jobs: number; agents: number };
interface FakeContext {
  sessionManager: { getSessionId(): string; getSessionFile(): string | undefined };
  signal: undefined;
  getBackgroundWork?: () => Counts;
}

/** A host context. `live` is the count `getBackgroundWork()` reports now; omit it for an older host. */
function fakeContext(live?: Counts): FakeContext {
  const context: FakeContext = {
    sessionManager: { getSessionId: () => "bg-wiring-session", getSessionFile: () => undefined },
    signal: undefined,
  };
  if (live) context.getBackgroundWork = () => ({ ...live });
  return context;
}

const globals = globalThis as typeof globalThis & {
  [RUNTIMES]?: Map<string, { driver: PiDriver; mesh: { stop(): Promise<void> } }>;
  [ONCE]?: { exit?: unknown };
};

const saved = Object.fromEntries(KEYS.map((key) => [key, process.env[key]]));
const oldExit = globals[ONCE]?.exit;
let checks = 0;
const ok = (condition: boolean, name: string): void => {
  assert.ok(condition, name);
  checks++;
};

const handlers = new Map<string, (...args: unknown[]) => unknown>();
const calls: string[] = [];

try {
  for (const key of KEYS) delete process.env[key];
  process.env.COTAL_NAME = "bg-wiring";
  process.env.COTAL_SPACE = "bg-wiring";
  process.env.COTAL_SERVERS = "nats://127.0.0.1:1";
  const api = {
    sendMessage(): void {},
    registerMessageRenderer(): void {},
    registerTool(): void {},
    on(event: string, handler: (...args: unknown[]) => unknown): void {
      handlers.set(event, handler);
    },
  } as unknown as ExtensionAPI;
  await cotalMesh(api);

  const runtime = globals[RUNTIMES]?.values().next().value;
  assert.ok(runtime, "cotalMesh registers one runtime");
  const driver = runtime.driver;
  // Stub the driver's other hooks so only the background reads reach the log.
  driver.onSessionStart = (): void => { calls.push("session_start"); };
  driver.onAgentStart = (): void => { calls.push("agent_start"); };
  driver.onToolEnd = (): void => { calls.push("tool_end"); };
  driver.onAgentEnd = (): void => { calls.push("agent_end"); };
  driver.onBackgroundWork = (jobs: number, agents: number): void => { calls.push(`background ${jobs} ${agents}`); };

  const emit = async (event: string, ...args: unknown[]): Promise<void> => {
    const handler = handlers.get(event);
    assert.ok(handler, `${event} is subscribed`);
    await handler(...args);
  };

  // The event subscription exists, under the host's event name.
  ok(handlers.has("background_work_changed"), "cotalMesh subscribes to background_work_changed");

  // M2: the event handler re-reads ctx.getBackgroundWork() and ignores a stale payload.
  const live: Counts = { jobs: 0, agents: 0 };
  calls.length = 0;
  await emit("background_work_changed", { jobs: 1, agents: 0 }, fakeContext(live));
  assert.deepEqual(calls, ["background 0 0"], "a stale payload of 1 job is ignored in favor of the live read of 0");
  checks++;
  live.jobs = 0; live.agents = 3;
  calls.length = 0;
  await emit("background_work_changed", { jobs: 0, agents: 0 }, fakeContext(live));
  assert.deepEqual(calls, ["background 0 3"], "a payload of zero is ignored in favor of the live read of 3 subagents");
  checks++;

  // The reads at each lifecycle hook carry the live count.
  live.jobs = 0; live.agents = 2;
  calls.length = 0;
  await emit("session_start", { reason: "startup" }, fakeContext(live));
  assert.deepEqual(calls, ["session_start", "background 0 2"], "session_start reads the live count");
  checks++;
  // A publisher host with no work reads zero at startup, so the seat publishes idle before any event.
  live.jobs = 0; live.agents = 0;
  calls.length = 0;
  await emit("session_start", { reason: "startup" }, fakeContext(live));
  assert.deepEqual(calls, ["session_start", "background 0 0"], "a fresh seat with no work reads zero at session start");
  checks++;
  live.jobs = 0; live.agents = 2;
  calls.length = 0;
  await emit("agent_start", {}, fakeContext(live));
  assert.deepEqual(calls, ["agent_start", "background 0 2"], "agent_start reads the live count");
  checks++;
  live.jobs = 1; live.agents = 0;
  calls.length = 0;
  await emit("tool_execution_end", {}, fakeContext(live));
  assert.deepEqual(calls, ["tool_end", "background 1 0"], "tool_execution_end reads the live count");
  checks++;
  live.jobs = 0; live.agents = 0;
  calls.length = 0;
  await emit("agent_end", { messages: [] }, fakeContext(live));
  assert.deepEqual(calls, ["agent_end", "background 0 0"], "agent_end reads the live count");
  checks++;

  // An older host has no getBackgroundWork: every read and the event stay silent and do not throw.
  calls.length = 0;
  await emit("session_start", { reason: "startup" }, fakeContext());
  await emit("agent_start", {}, fakeContext());
  await emit("tool_execution_end", {}, fakeContext());
  await emit("agent_end", { messages: [] }, fakeContext());
  await emit("background_work_changed", { jobs: 1, agents: 0 }, fakeContext());
  assert.deepEqual(calls, ["session_start", "agent_start", "tool_end", "agent_end"], "an older host reports no background count");
  checks++;

  console.log(`background-wiring smoke: ${checks} checks passed`);
} finally {
  const runtimes = globals[RUNTIMES];
  if (runtimes) {
    for (const runtime of runtimes.values()) await runtime.mesh.stop();
    runtimes.clear();
  }
  if (globals[ONCE]) globals[ONCE].exit = oldExit;
  for (const key of KEYS) {
    const value = saved[key];
    if (value === undefined) delete process.env[key];
    else process.env[key] = value;
  }
}
