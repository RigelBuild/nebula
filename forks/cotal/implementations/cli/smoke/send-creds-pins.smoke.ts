/**
 * `cotal send --creds` names a sender from `<creds>.pins.json` only when no seed record exists. A seed
 * record is final; a pinned account JWT that does not bind its keys, or revokes, expires or defers the cred, falls back.
 *
 * Run: pnpm smoke:send-creds-pins
 */
import { spawn } from "node:child_process";
import { randomUUID } from "node:crypto";
import { existsSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { hostname, tmpdir, userInfo } from "node:os";
import { join } from "node:path";
import { CotalEndpoint, createSpaceAuth, decodeNatsJwt, isReachable, mintCreds, newIdentity, provisionAgent, revokeAccountUser, seedChannelRegistry, serverConfig, setupSpaceStreams, mintLifecycleUid, type CotalMessage, type Delivery, type SpaceAuth } from "@cotal-ai/core";
import { authDir, recordMesh, saveSpaceAuth } from "@cotal-ai/workspace";
import { encodeAccount } from "@nats-io/jwt";
import { fromPublic, fromSeed } from "@nats-io/nkeys";
import { createSuite, freePort, killAndAwaitExit, teardownOnSignal, awaitBrokerReady } from "@cotal-ai/smoke-kit";

const BIN = join(import.meta.dirname, "..", "..", "..", "bin", "dist", "cotal.js");
if (!existsSync(BIN)) {
  console.error(`✗ ${BIN} missing - build first: pnpm --filter cotal-ai... build`);
  process.exit(1);
}

// `Promise.withResolvers` needs lib es2024; the smoke tsconfig lib stops at es2023, so build the pair by hand.
function deferred<T>(): { promise: Promise<T>; resolve: (value: T | PromiseLike<T>) => void; reject: (reason?: unknown) => void } {
  let resolve!: (value: T | PromiseLike<T>) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

const { check, finish } = createSuite();
const space = `credspins-${randomUUID().slice(0, 8)}`;
const dir = mkdtempSync(join(tmpdir(), "cotal-creds-pins-"));
const home = join(dir, "home");
const operatorShell = join(dir, "shell");
mkdirSync(join(home, ".cotal"), { recursive: true, mode: 0o700 });
mkdirSync(operatorShell, { recursive: true });

// No meshes registry and no seeds unless a case records one. Only the creds and their pins sidecar can prove a name.
const HOST_ENV = Object.fromEntries(Object.entries(process.env).filter(([k]) => !k.startsWith("COTAL_") && k !== "FORCE_COLOR" && k !== "NO_COLOR"));
const cotalHome = join(home, ".cotal");
const env = { ...HOST_ENV, HOME: home, USERPROFILE: home, XDG_CONFIG_HOME: join(home, ".config"), COTAL_HOME: cotalHome, COTAL_SKIP_CONNECTOR_SEED: "1", NO_COLOR: "1" };

const auth = await createSpaceAuth(space);
const port = await freePort();
const servers = `nats://127.0.0.1:${port}`;
writeFileSync(join(dir, "server.conf"), serverConfig(auth, [auth], { transport: { kind: "plaintext" }, port, storeDir: join(dir, "js") }));
const broker = spawn("nats-server", ["-c", join(dir, "server.conf")], { stdio: "ignore" });
const releaseBroker = teardownOnSignal(broker, dir);

const expectedSender = `${userInfo().username}@${hostname()}`;
const got: Array<{ text: string; fromName: string }> = [];
const waiters = new Map<string, () => void>();
const onMessage = (message: CotalMessage, delivery: Delivery) => {
  const text = message.parts.map((part) => (part.kind === "text" ? part.text : "")).join("");
  got.push({ text, fromName: message.from.name });
  delivery.ack();
  waiters.get(text)?.();
};

// Async, so bob's event loop keeps delivering while the send runs.
const run = (args: string[]) => new Promise<{ status: number | null; stdout: string; stderr: string }>((resolveRun) => {
  const child = spawn(process.execPath, [BIN, ...args], { cwd: operatorShell, env });
  let stdout = "";
  let stderr = "";
  child.stdout.on("data", (chunk: Buffer) => { stdout += chunk; });
  child.stderr.on("data", (chunk: Buffer) => { stderr += chunk; });
  const timer = setTimeout(() => child.kill("SIGKILL"), 60_000);
  child.on("close", (status) => { clearTimeout(timer); resolveRun({ status, stdout, stderr }); });
});

// Runs `action` with COTAL_HOME pointed at this smoke's home, as send.smoke does.
async function withCotalHome<T>(action: () => T): Promise<T> {
  const previous = process.env.COTAL_HOME;
  process.env.COTAL_HOME = cotalHome;
  try {
    return action();
  } finally {
    if (previous === undefined) delete process.env.COTAL_HOME;
    else process.env.COTAL_HOME = previous;
  }
}

// The space's account JWT with one claim changed, re-signed by its operator. Only that claim differs.
function resignAccountJwt(space: SpaceAuth, change: (claims: Record<string, unknown>) => void): string {
  const body = space.account.jwt.split(".")[1] ?? "";
  const claims = JSON.parse(Buffer.from(body, "base64url").toString("utf8")) as Record<string, unknown>;
  change(claims);
  const header = Buffer.from(JSON.stringify({ typ: "JWT", alg: "ed25519-nkey" })).toString("base64url");
  const payload = Buffer.from(JSON.stringify(claims)).toString("base64url");
  const operator = fromSeed(new TextEncoder().encode(space.operator.seed));
  const signature = Buffer.from(operator.sign(new TextEncoder().encode(`${header}.${payload}`))).toString("base64url");
  operator.clear();
  return `${header}.${payload}.${signature}`;
}

// The space's account JWT re-signed by the operator with an expiry in the past.
async function encodeExpiredAccountJwt(space: SpaceAuth): Promise<string> {
  const operator = fromSeed(new TextEncoder().encode(space.operator.seed));
  return encodeAccount(space.space, fromPublic(space.account.pub), { signing_keys: [space.account.signingPub] }, { signer: operator, exp: 1 });
}

// A different account's JWT, signed by its own operator: self-consistent, but not this space's.
const otherAuth = await createSpaceAuth(`${space}-other`);

let bob: CotalEndpoint | undefined;
try {
  await awaitBrokerReady(() => isReachable(servers), { servers, attempts: 25, delayMs: 200 });
  const provisionerCreds = await mintCreds(auth, newIdentity(), "provisioner");
  await setupSpaceStreams({ servers, space, creds: provisionerCreds });
  await seedChannelRegistry({ servers, space, creds: provisionerCreds, file: { channels: { general: {} } } });
  const provisioner = new CotalEndpoint({ space, servers, creds: provisionerCreds, card: { name: "pins-provisioner", kind: "endpoint" }, consume: false, watchPresence: false, registerPresence: false });
  await provisioner.start();

  const bobIdentity = newIdentity();
  const bobUid = mintLifecycleUid();
  const bobCreds = await provisionAgent(provisioner, auth, bobIdentity, { lifecycleUid: bobUid, role: "reviewer", subscribe: ["general"], allowSubscribe: ["general"] });
  bob = new CotalEndpoint({ space, servers, creds: bobCreds, lifecycleUid: bobUid, card: { name: "bob", role: "reviewer", kind: "agent", id: bobIdentity.id }, channels: ["general"], heartbeatMs: 500, ttlMs: 10_000 });
  bob.on("message", onMessage);
  bob.on("error", (error: Error) => console.error("! bob:", error.message));
  await bob.start();
  // A presence observer must see bob before any `--to bob` send, as send.smoke's observer does.
  const adminCreds = await mintCreds(auth, newIdentity(), "admin");
  const observer = new CotalEndpoint({ space, servers, creds: adminCreds, card: { name: "pins-observer", kind: "endpoint" }, channels: [], consume: false, registerPresence: false, watchPresence: true, watchChannels: false });
  observer.on("error", () => {});
  await observer.start();
  const seen = (await observer.waitForPresenceSnapshot(5_000)) === "snapshot" && observer.getRoster().some((p) => p.card.name === "bob");
  await observer.stop();
  check("bob is visible in the presence view before the sends", seen);

  // A public-only pins file: the space's account and signing public keys, the account JWT, and no seeds.
  const pins = { space, accountPub: auth.account.pub, signingPub: auth.account.signingPub, accountJwt: auth.account.jwt };
  const pinsJson = JSON.stringify(pins);

  // Resolves when bob gets `text`. The deadline is armed after the send exits, and fails only this case.
  const deliveredWithin = (text: string, deadlineMs: number) => {
    if (got.some((m) => m.text === text)) return Promise.resolve();
    const { promise, resolve, reject } = deferred<void>();
    const timer = setTimeout(() => { waiters.delete(text); reject(new Error(`no delivery of "${text}" within ${deadlineMs}ms`)); }, deadlineMs);
    waiters.set(text, () => { clearTimeout(timer); waiters.delete(text); resolve(); });
    return promise;
  };
  const send = async (creds: string, text: string) => {
    const result = await run(["send", "--to", "bob", text, "--space", space, "--server", servers, "--creds", creds]);
    await deliveredWithin(text, 10_000).catch(() => {});
    return { result, delivered: got.find((m) => m.text === text) };
  };
  const writeCreds = async (name: string, owner: SpaceAuth) => {
    const path = join(dir, `${name}.creds`);
    writeFileSync(path, await mintCreds(owner, newIdentity(), "operator", { name }), { mode: 0o600 });
    return path;
  };
  const writePins = (name: string, body: string) => writeFileSync(join(dir, `${name}.pins.json`), body, { mode: 0o600 });

  // 1. A cred whose key and JWT match a pin takes its proven name from the pins file alone.
  const provenCreds = await writeCreds("nemo-alerts", auth);
  writePins("nemo-alerts", pinsJson);
  const provenText = `proven-${randomUUID().slice(0, 6)}`;
  const proven = await send(provenCreds, provenText);
  check("a pinned cred with no seeds on the host takes its proven name from its pins file",
    proven.result.status === 0 && proven.delivered?.fromName === "nemo-alerts", { proven, got });

  // 2. A cred not in the pins falls back to login@host.
  const strangerCreds = await writeCreds("stranger", auth);
  writePins("stranger", JSON.stringify({ ...pins, accountPub: otherAuth.account.pub, signingPub: otherAuth.account.signingPub, accountJwt: otherAuth.account.jwt }));
  const strangerText = `stranger-${randomUUID().slice(0, 6)}`;
  const stranger = await send(strangerCreds, strangerText);
  check("a cred that is not in its pins falls back to login@host",
    stranger.result.status === 0 && stranger.delivered?.fromName === expectedSender, { stranger, got });

  // 3. A pinned cred whose account JWT revokes its key falls back to login@host.
  const revokedIdentity = newIdentity();
  const revokedCreds = join(dir, "revoked.creds");
  writeFileSync(revokedCreds, await mintCreds(auth, revokedIdentity, "operator", { name: "revoked-sender" }), { mode: 0o600 });
  const revokedAuth = await revokeAccountUser(auth, revokedIdentity.id, Math.floor(Date.now() / 1000) + 1);
  writePins("revoked", JSON.stringify({ ...pins, accountJwt: revokedAuth.account.jwt }));
  const revokedText = `revoked-${randomUUID().slice(0, 6)}`;
  const revoked = await send(revokedCreds, revokedText);
  check("a revoked cred in its pins falls back to login@host",
    revoked.result.status === 0 && revoked.delivered?.fromName === expectedSender, { revoked, got });

  // 4. An expired pinned account JWT falls back to login@host. The cred is live, so only the JWT's expiry can cause it.
  const expiredCreds = await writeCreds("expired", auth);
  writePins("expired", JSON.stringify({ ...pins, accountJwt: await encodeExpiredAccountJwt(auth) }));
  const expiredText = `expired-${randomUUID().slice(0, 6)}`;
  const expired = await send(expiredCreds, expiredText);
  check("a cred whose pinned account JWT is expired falls back to login@host",
    expired.result.status === 0 && expired.delivered?.fromName === expectedSender, { expired, got });

  // 5. A pinned account JWT not yet valid (nbf one hour ahead) falls back to login@host.
  const futureCreds = await writeCreds("future", auth);
  writePins("future", JSON.stringify({ ...pins, accountJwt: resignAccountJwt(auth, (c) => { c.nbf = Math.floor(Date.now() / 1000) + 3600; }) }));
  const futureText = `future-${randomUUID().slice(0, 6)}`;
  const future = await send(futureCreds, futureText);
  check("a cred whose pinned account JWT is not yet valid falls back to login@host",
    future.result.status === 0 && future.delivered?.fromName === expectedSender, { future, got });

  // 6. A pinned account JWT whose exp is a string falls back to login@host. The token is re-signed by
  // hand: the precondition check proves the operator signature verifies and exp is the string.
  const stringExpJwt = resignAccountJwt(auth, (c) => { c.exp = "never"; });
  let stringExpSigned = false;
  try { stringExpSigned = decodeNatsJwt<{ exp?: unknown }>(stringExpJwt).exp === "never"; } catch { /* the check below fails */ }
  check("the string-exp pinned JWT verifies under the operator and carries exp as \"never\"", stringExpSigned);
  const stringExpCreds = await writeCreds("string-exp", auth);
  writePins("string-exp", JSON.stringify({ ...pins, accountJwt: stringExpJwt }));
  const stringExpText = `string-exp-${randomUUID().slice(0, 6)}`;
  const stringExp = await send(stringExpCreds, stringExpText);
  check("a cred whose pinned account JWT has a non-numeric exp falls back to login@host",
    stringExp.result.status === 0 && stringExp.delivered?.fromName === expectedSender, { stringExp, got });

  // 7. Another account's JWT, substituted for this space's, falls back to login@host. Its revocations and
  // expiry must not be read as this space's, and its name must not be taken.
  const substitutedCreds = await writeCreds("substituted", auth);
  writePins("substituted", JSON.stringify({ ...pins, accountJwt: otherAuth.account.jwt }));
  const substitutedText = `substituted-${randomUUID().slice(0, 6)}`;
  const substituted = await send(substitutedCreds, substitutedText);
  check("a cred whose pins carry another account's JWT falls back to login@host",
    substituted.result.status === 0 && substituted.delivered?.fromName === expectedSender, { substituted, got });

  // 8. Guard: a malformed pins file falls back to login@host with exit 0 and no TypeError on stderr.
  // A throw that escapes the pins read is not visible here, because the CLI's own try hides it.
  const malformedCreds = await writeCreds("malformed", auth);
  writePins("malformed", "{ not json");
  const malformedText = `malformed-${randomUUID().slice(0, 6)}`;
  const malformed = await send(malformedCreds, malformedText);
  check("a malformed pins file falls back to login@host without throwing from the send",
    malformed.result.status === 0 && malformed.delivered?.fromName === expectedSender && !malformed.result.stderr.includes("TypeError"), { malformed, got });

  // 8b. A space registered by `cotal meshes add` (origin "manual") at a root with no seed record. The pins
  // file must still name a cred it admits: no seed record exists, so the pins decide.
  const noSeedRoot = join(dir, "no-seed-root");
  await withCotalHome(() => {
    recordMesh({ space, server: servers, root: noSeedRoot, mode: "auth", origin: "manual", ts: new Date().toISOString() });
  });
  const noSeedCreds = await writeCreds("no-seed", auth);
  writePins("no-seed", pinsJson);
  const noSeedText = `no-seed-${randomUUID().slice(0, 6)}`;
  const noSeed = await send(noSeedCreds, noSeedText);
  check("a registered space with no seed record takes the cred's name from its pins file",
    noSeed.result.status === 0 && noSeed.delivered?.fromName === "no-seed", { noSeed, got });

  // 9. A registered seed record for this space refuses a cred the pins would admit. The seed is final,
  // so the send falls back to login@host and does not take the pins' name. The seed uses other keys.
  const seedRefusedAuth = await createSpaceAuth(space);
  const seedRefusedRoot = join(dir, "seed-refused-root");
  await withCotalHome(() => {
    saveSpaceAuth(authDir(seedRefusedRoot), seedRefusedAuth);
    recordMesh({ space, server: servers, root: seedRefusedRoot, mode: "auth", origin: "manual", ts: new Date().toISOString() });
  });
  const seedRefusedCreds = await writeCreds("seed-refused", auth);
  writePins("seed-refused", pinsJson);
  const seedRefusedText = `seed-refused-${randomUUID().slice(0, 6)}`;
  const seedRefused = await send(seedRefusedCreds, seedRefusedText);
  check("a registered seed record that refuses a cred falls back to login@host, not to the pins",
    seedRefused.result.status === 0 && seedRefused.delivered?.fromName === expectedSender, { seedRefused, got });

  // The registered seed now admits this space's own keys. Cells 10 and 11 run against it.
  const seedAdmittedRoot = join(dir, "seed-admitted-root");
  await withCotalHome(() => {
    saveSpaceAuth(authDir(seedAdmittedRoot), auth);
    recordMesh({ space, server: servers, root: seedAdmittedRoot, mode: "auth", origin: "manual", ts: new Date().toISOString() });
  });

  // 10. Guard: a seed record and pins that both admit a cred give the cred's own name.
  const bothCreds = await writeCreds("both-admit", auth);
  writePins("both-admit", pinsJson);
  const bothText = `both-admit-${randomUUID().slice(0, 6)}`;
  const both = await send(bothCreds, bothText);
  check("a seed record and pins that both admit a cred give the cred's name",
    both.result.status === 0 && both.delivered?.fromName === "both-admit", { both, got });

  // 11. Guard: a seed record that admits a cred gives its name even when the pins revoke it. Pins
  // that take precedence over the seed would fall back here.
  const seedAdmitIdentity = newIdentity();
  const pinsRevokedCreds = join(dir, "seed-pins-revoked.creds");
  writeFileSync(pinsRevokedCreds, await mintCreds(auth, seedAdmitIdentity, "operator", { name: "seed-pins-revoked" }), { mode: 0o600 });
  const pinsRevokedAuth = await revokeAccountUser(auth, seedAdmitIdentity.id, Math.floor(Date.now() / 1000) + 1);
  writePins("seed-pins-revoked", JSON.stringify({ ...pins, accountJwt: pinsRevokedAuth.account.jwt }));
  const pinsRevokedText = `seed-pins-revoked-${randomUUID().slice(0, 6)}`;
  const pinsRevoked = await send(pinsRevokedCreds, pinsRevokedText);
  check("a seed record that admits a cred gives its name even when the pins revoke it",
    pinsRevoked.result.status === 0 && pinsRevoked.delivered?.fromName === "seed-pins-revoked", { pinsRevoked, got });
} finally {
  await bob?.stop().catch(() => {});
  await killAndAwaitExit(broker).catch(() => {});
  releaseBroker();
  rmSync(dir, { recursive: true, force: true });
}

finish();
process.exit(process.exitCode ?? 0);
