/**
 * `cotal ps --json` seats rows carry `background` while the seat's presence card reports
 * `meta.background` as `running` or `idle` (SPEC §6). A seat without the publisher carries no key, so
 * its row omits `background`. The row is built by the manager's real `list()`, so the test drives that
 * method directly over a roster it controls: no broker, no spawned seat.
 * Run: tsx smoke/ps-background.smoke.ts
 */
import { strict as assert } from "node:assert";
import { DEV_OWNER, principalKey } from "@cotal-ai/core";
import { Manager } from "../src/manager.js";

type Row = { name: string; background?: string; provider?: string };
type Lister = {
  list(ownerFilter?: string): Row[];
  agents: Map<string, unknown>;
  ep: { getRoster(): unknown[] };
  space: string;
  workspaceRoot: string;
};

const LIFECYCLE_UID = "a".repeat(26);
// A static seat's id is its raw token; its presence principal is the derived DEV_OWNER dot-form.
const SEAT_TOKEN = "bgseat";
const PRINCIPAL = principalKey(DEV_OWNER, SEAT_TOKEN).key;

function seatRow(meta: Record<string, unknown> | undefined) {
  return {
    card: { id: PRINCIPAL, name: SEAT_TOKEN, kind: "agent", ...(meta ? { meta } : {}) },
    status: "idle",
    ts: Date.now(),
    lifecycleUid: LIFECYCLE_UID,
  };
}

function rowFor(roster: unknown[]): Row | undefined {
  const manager = Object.create(Manager.prototype) as Lister;
  const seat = {
    name: SEAT_TOKEN,
    id: SEAT_TOKEN,
    agent: "pi",
    handle: { kind: "pty", status: () => "running", pid: 4242 },
    startedAt: Date.now(),
    lifecycleUid: LIFECYCLE_UID,
    launch: { model: undefined, variant: undefined, cwd: "/tmp" },
    spawner: undefined,
  };
  manager.agents = new Map([[SEAT_TOKEN, seat]]);
  manager.ep = { getRoster: () => roster };
  manager.space = "ps-background";
  manager.workspaceRoot = "/tmp";
  return manager.list().find((r) => r.name === SEAT_TOKEN);
}

let passed = 0;
const check = (name: string, ok: boolean) => {
  assert.ok(ok, name);
  passed++;
};

// A running seat publishes background = "running" on its row.
check(
  "a running seat's ps row carries background running",
  rowFor([seatRow({ background: "running" })])?.background === "running",
);

// A seat that has the publisher and no work reports idle on its row.
{
  const row = rowFor([seatRow({ background: "idle" })]);
  check("an idle seat's ps row carries background idle", row?.background === "idle");
}

// A seat without the publisher carries no background key, so its row omits background.
{
  const row = rowFor([seatRow(undefined)]);
  check("a seat without the publisher omits background", row !== undefined && !("background" in row));
}

// A value other than running or idle is never published, so a stray value cannot leak onto the row.
{
  const row = rowFor([seatRow({ background: "stopped" })]);
  check("a background value other than running or idle is not published", row !== undefined && !("background" in row));
}

// The sibling provider field publishes alongside background, unaffected by it.
{
  const row = rowFor([seatRow({ provider: "anthropic", background: "running" })]);
  check("provider and background publish together", row?.provider === "anthropic" && row?.background === "running");
}

console.log(`ps-background smoke: ${passed} checks passed`);
