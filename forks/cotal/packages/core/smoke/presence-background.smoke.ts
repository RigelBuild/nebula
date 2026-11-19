/**
 * Presence background-work test (no runner). `setCardBackground` overlays `meta.background` on the
 * card and republishes presence. It sets `"running"` while work runs, removes the key when none
 * runs, and writes nothing when the value is unchanged. Reads the RAW presence KV record, so the
 * assertion is on what a wire consumer sees, not on the local card object.
 * Run: tsx smoke/presence-background.smoke.ts
 */
import { strict as assert } from "node:assert";
import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { connect } from "@nats-io/transport-node";
import { Kvm } from "@nats-io/kv";
import { CotalEndpoint, DEV_OWNER, isReachable, mintLifecycleUid, presenceBucket, principalKey, type Presence } from "../src/index.js";
import { pickFreePort } from "./_free-port.js";
import { SMOKE_BROKER_TOKEN, awaitBrokerReady, teardownOnSignal } from "@cotal-ai/smoke-kit";

const PORT = await pickFreePort();
const servers = `nats://127.0.0.1:${PORT}`;
const space = "backgroundsmoke";
const SEAT_TOKEN = "bgseat";
// The card id is a plain token; the presence KV key is the derived DEV_OWNER dot-form.
const key = principalKey(DEV_OWNER, SEAT_TOKEN).key;
let passed = 0;
const check = (name: string, ok: boolean) => {
  if (!ok) throw new Error(`FAIL: ${name}`);
  passed++;
};

const directory = mkdtempSync(join(tmpdir(), SMOKE_BROKER_TOKEN));
const broker = spawn("nats-server", ["-js", "-a", "127.0.0.1", "-p", String(PORT), "-sd", join(directory, "js")], { stdio: "ignore" });
const release = teardownOnSignal(broker, directory);
let seat: CotalEndpoint | undefined;
try {
  await awaitBrokerReady(() => isReachable(servers), { servers, attempts: 50, delayMs: 100 });
  seat = new CotalEndpoint({ space, servers, channels: [], consume: false, watchPresence: false, watchChannels: false,
    heartbeatMs: 60_000, ttlMs: 120_000, card: { id: SEAT_TOKEN, name: SEAT_TOKEN, kind: "agent" }, lifecycleUid: mintLifecycleUid() });
  seat.on("error", () => {});
  await seat.start();
  const nc = await connect({ servers });
  try {
    const kv = await new Kvm(nc).open(presenceBucket(space));
    const read = async (): Promise<Presence> => {
      const row = await kv.get(key);
      assert.ok(row, "the presence row exists");
      return row.json<Presence>();
    };

    await seat.setCardCwd("/work/bg");
    await seat.setCardBackground(true);
    const running = await read();
    check("true sets meta.background to running on the wire", running.card.meta?.background === "running");
    const revision = (await kv.get(key))!.revision;
    await seat.setCardBackground(true);
    check("an unchanged true writes no new revision", (await kv.get(key))!.revision === revision);

    await seat.setCardBackground(false);
    const idle = await read();
    check("false removes the meta.background key on the wire", !("background" in (idle.card.meta ?? {})));
    check("removing the key keeps other card meta", idle.card.meta?.cwd === "/work/bg");

    const cleared = (await kv.get(key))!.revision;
    await seat.setCardBackground(false);
    check("clearing an already-clear card writes nothing", (await kv.get(key))!.revision === cleared);
    await seat.setCardBackground(true);
    await seat.setStatus("offline");
    const gone = await read();
    check("an offline record drops meta.background and keeps other card meta", gone.status === "offline" && !("background" in (gone.card.meta ?? {})) && gone.card.meta?.cwd === "/work/bg");
    console.log(`presence-background smoke: ${passed} checks passed`);
  } finally {
    await nc.close();
  }
} finally {
  await seat?.stop().catch(() => {});
  release();
  rmSync(directory, { recursive: true, force: true });
}
