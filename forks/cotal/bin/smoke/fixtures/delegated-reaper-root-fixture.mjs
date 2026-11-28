import { spawn } from "node:child_process";
import { createHash } from "node:crypto";
import { createServer } from "node:net";
import { existsSync, writeFileSync } from "node:fs";
import { basename, dirname, join } from "node:path";
import { tmpdir } from "node:os";
import { captureSpawnedIdentity, signalByPidfd } from "../reaper-pidfd.mjs";

const [runMarker, scope, dir] = process.argv.slice(2);
if (!runMarker || !scope || !dir || process.getuid?.() !== 0) {
  console.error("fixture requires run, scope, fixture dir, and root uid");
  process.exit(2);
}

let broker;
let custodian;
let differentMarkerCustodian;
let reported = false;
let cleaned = false;
const processRecords = [];
const manifestPath = join(dir, "processes.json");
async function recordChild(child, marker) {
  const record = await captureSpawnedIdentity(child, marker);
  processRecords.push({ ...record, marker });
  writeFileSync(manifestPath, JSON.stringify(processRecords));
  return record;
}
function cleanup() {
  if (cleaned) return [];
  cleaned = true;
  const failures = [];
  for (const record of processRecords) {
    if (!record.startTime || !record.command) {
      failures.push(`pid ${record.pid}: missing recorded process identity; refusing to signal`);
      continue;
    }
    const signal = signalByPidfd(record.pid, { command: record.command, startTime: record.startTime }, [record.marker]);
    if (signal.status === "failed") failures.push(`pid ${record.pid}: ${signal.error ?? signal.status}`);
  }
  for (const failure of failures) console.error(`fixture cleanup: ${failure}`);
  return failures;
}
process.on("exit", () => { if (!reported) cleanup(); });

process.on("SIGTERM", () => { process.exit(cleanup().length > 0 ? 1 : 0); });
process.on("SIGINT", () => { process.exit(cleanup().length > 0 ? 1 : 0); });

try {
  const port = await new Promise((resolve, reject) => {
    const server = createServer();
    server.once("error", reject);
    server.listen(0, "127.0.0.1", () => {
      const address = server.address();
      if (!address || typeof address === "string") return reject(new Error("no test port"));
      server.close((error) => error ? reject(error) : resolve(address.port));
    });
  });
  const digest = createHash("sha256").update(scope.trim()).digest().subarray(0, 16).toString("base64url");
  const expectedPrefix = `cotal-smoke-broker-2147483647-s${digest}-`;
  if (dirname(dir) !== tmpdir() || !basename(dir).startsWith(expectedPrefix) || !existsSync(dir)) throw new Error("fixture dir is not the parent-created scoped directory");
  writeFileSync(join(dir, "server.conf"), `port: ${port}\njetstream { store_dir: "${join(dir, "js")}" }\n`);
  broker = spawn("nats-server", ["-c", join(dir, "server.conf")], { stdio: "ignore" });
  const brokerRecord = await recordChild(broker, dir);
  custodian = spawn(process.execPath, ["-e", "setInterval(() => {}, 1000)", "--", "--cotal-run", runMarker, "/tmp/dist/custodian.js"], { stdio: "ignore" });
  const custodianRecord = await recordChild(custodian, runMarker);
  const differentMarker = `${runMarker}-different-marker`;
  differentMarkerCustodian = spawn(process.execPath, ["-e", "setInterval(() => {}, 1000)", "--", "--cotal-run", differentMarker, "/tmp/dist/custodian.js"], { stdio: "ignore" });
  const differentMarkerCustodianRecord = await recordChild(differentMarkerCustodian, differentMarker);
  broker.unref();
  custodian.unref();
  differentMarkerCustodian.unref();
  await new Promise((resolve) => setTimeout(resolve, 500));
  console.log("__ROOT_FIXTURE__" + JSON.stringify({
    brokerPid: brokerRecord.pid,
    brokerStartTime: brokerRecord.startTime,
    custodianPid: custodianRecord.pid,
    custodianStartTime: custodianRecord.startTime,
    differentMarkerCustodianPid: differentMarkerCustodianRecord.pid,
    differentMarkerCustodianStartTime: differentMarkerCustodianRecord.startTime,
    differentMarker,
    processes: processRecords,
    dir,
    uid: process.getuid(),
  }));
  reported = true;
  await new Promise((resolve) => {
    process.stdin.setEncoding("utf8");
    process.stdin.on("data", (chunk) => { if (chunk.includes("cleanup")) resolve(); });
  });
  const cleanupFailures = await cleanup();
  if (cleanupFailures.length > 0) {
    process.exitCode = 1;
    throw new Error(`fixture cleanup failed: ${cleanupFailures.join("; ")}`);
  }
  console.log("__ROOT_FIXTURE_CLEANED__");
} catch (error) {
  await cleanup();
  throw error;
}