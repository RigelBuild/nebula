import assert from "node:assert/strict";
import { test } from "node:test";
import { readProcessIdentity, signalByPidfd } from "./reaper-pidfd.mjs";

const supported = { supported: true };
const scannedIdentity = { command: "nats-server", startTime: "100" };

test("pidfd signaling refuses a PID whose start identity changed after the scan", () => {
  let currentIdentity = scannedIdentity;
  let signalAttempted = false;
  const result = signalByPidfd(2345, scannedIdentity, ["cotal-smoke-broker-"], {
    beforeSignal() {
      // Model the scanned process exiting and a replacement receiving the same PID.
      currentIdentity = { command: "nats-server", startTime: "101" };
    },
    readIdentity() { return currentIdentity; },
    checkSupport() { return supported; },
    runSignal() {
      signalAttempted = true;
      return { status: 0, stdout: "signaled", stderr: "", error: undefined };
    },
  });
  assert.equal(result.status, "failed");
  assert.match(result.error ?? "", /identity changed since scan/);
  assert.equal(signalAttempted, false);
});

test("pidfd signaling refuses missing start identity without invoking the signal helper", () => {
  let signalAttempted = false;
  const result = signalByPidfd(2345, undefined, ["--cotal-run", "run-1"], {
    readIdentity() { return { command: "node", startTime: "100" }; },
    runSignal() {
      signalAttempted = true;
      return { status: 0, stdout: "signaled", stderr: "", error: undefined };
    },
  });
  assert.equal(result.status, "failed");
  assert.match(result.error ?? "", /missing process identity/);
  assert.equal(signalAttempted, false);
});
test("spawn identity capture kills and waits for its owned child when start identity is missing", async () => {
  const { captureSpawnedIdentity } = await import("./reaper-pidfd.mjs");
  let exitListener;
  let killCount = 0;
  const child = {
    pid: 2345,
    exitCode: null,
    signalCode: null,
    kill() {
      killCount++;
      queueMicrotask(() => exitListener?.());
      return true;
    },
    once(event, listener) {
      assert.equal(event, "exit");
      exitListener = listener;
      return this;
    },
  };
  await assert.rejects(captureSpawnedIdentity(child, "run-1", { readIdentity: () => undefined }), /start identity unavailable/);
  assert.equal(killCount, 1);
});

test("pidfd signaling reports unsupported without invoking the signal helper", () => {
  let signalAttempted = false;
  const result = signalByPidfd(2345, scannedIdentity, ["cotal-smoke-broker-"], {
    readIdentity() { return scannedIdentity; },
    checkSupport() { return { supported: false, error: "pidfd probe unavailable" }; },
    runSignal() {
      signalAttempted = true;
      return { status: 0, stdout: "signaled", stderr: "", error: undefined };
    },
  });
  assert.equal(result.status, "failed");
  assert.equal(result.error, "pidfd probe unavailable");
  assert.equal(signalAttempted, false);
});

test("process identity reader preserves stat command names containing closing parens", () => {
  const fields = ["S", "1", "0", "-1", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "9876"];
  const identity = readProcessIdentity(123, { readStat: () => `123 (custodian ) with parens) ${fields.join(" ")}\n` });
  assert.deepEqual(identity, { command: "custodian ) with parens", startTime: "9876" });
});
