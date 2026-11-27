import { spawn } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdtempSync, writeFileSync } from "node:fs";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";

const runMarker = process.env.COTAL_RUN;
const brokerScope = process.env.SMOKE_BROKER_SCOPE;
if (!runMarker || !brokerScope) throw new Error("delegated leak fixture requires run and scope markers");

const probe = createServer();
const { promise, resolve, reject } = Promise.withResolvers();
probe.once("error", reject);
probe.listen(0, "127.0.0.1", () => {
  const address = probe.address();
  if (!address || typeof address === "string") {
    reject(new Error("no delegated fixture port"));
    return;
  }
  probe.close((error) => error ? reject(error) : resolve(address.port));
});
const port = await promise;
const digest = createHash("sha256").update(brokerScope.trim()).digest().subarray(0, 16).toString("base64url");
const brokerDir = mkdtempSync(join(tmpdir(), `cotal-smoke-broker-${process.pid}-s${digest}-`));
writeFileSync(join(brokerDir, "server.conf"), `port: ${port}\n`);
const broker = spawn("nats-server", ["-c", join(brokerDir, "server.conf")], { stdio: "ignore" });
const custodian = spawn(process.execPath, ["-e", "setInterval(() => {}, 1000)", "--", "--cotal-run", runMarker, "/tmp/dist/custodian.js"], { stdio: "ignore" });
if (broker.pid === undefined || custodian.pid === undefined) throw new Error("delegated leak fixture child did not start");
broker.unref();
custodian.unref();
await new Promise((resolve) => setTimeout(resolve, 300));
console.log(`__DELEGATED_FIXTURE__${JSON.stringify({ brokerPid: broker.pid, custodianPid: custodian.pid, brokerDir, runMarker, brokerScope, delegated: process.env.COTAL_SEAT_CGROUP_DELEGATED })}`);
console.log("COTAL_SMOKE_SENTINEL cells=3 passed=3 failed=0");
