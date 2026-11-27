import { spawn } from "node:child_process";
import { createHash } from "node:crypto";
import { createServer } from "node:net";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const [runMarker, scope] = process.argv.slice(2);
if (!runMarker || !scope || process.getuid?.() !== 0) {
  console.error("fixture requires a run marker, broker scope, and root uid");
  process.exit(2);
}

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
const dir = mkdtempSync(join(tmpdir(), `cotal-smoke-broker-${process.pid}-s${digest}-`));
writeFileSync(join(dir, "server.conf"), `port: ${port}\njetstream { store_dir: "${join(dir, "js")}" }\n`);
const broker = spawn("nats-server", ["-c", join(dir, "server.conf")], { stdio: "ignore" });
const custodian = spawn(process.execPath, ["-e", "setInterval(() => {}, 1000)", "--", "--cotal-run", runMarker, "/tmp/dist/custodian.js"], { stdio: "ignore" });
if (broker.pid === undefined || custodian.pid === undefined) throw new Error("fixture child did not start");
broker.unref();
custodian.unref();
await new Promise((resolve) => setTimeout(resolve, 500));
console.log(JSON.stringify({ brokerPid: broker.pid, custodianPid: custodian.pid, dir, uid: process.getuid() }));