import { Database } from "bun:sqlite";
import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import { AuthStorage, SqliteAuthCredentialStore } from "@oh-my-pi/pi-ai";
import { type AuthBrokerServerHandle, startAuthBroker } from "@oh-my-pi/pi-ai/auth-broker";
import { removeWithRetries } from "../../utils/src/temp";

const refreshOAuthCredential = mock(async () => {
	throw new Error("unexpected upstream refresh");
});

const TOKEN = "auth-broker-readyz-bearer";

describe("auth-broker GET /v1/readyz", () => {
	let tempDir = "";
	let store: SqliteAuthCredentialStore | undefined;
	let storage: AuthStorage | undefined;
	let handle: AuthBrokerServerHandle | undefined;
	const upstreamRefreshMock = refreshOAuthCredential;

	beforeEach(async () => {
		tempDir = await fs.mkdtemp(path.join(os.tmpdir(), "auth-broker-readyz-"));
		store = await SqliteAuthCredentialStore.open(path.join(tempDir, "agent.db"));
		upstreamRefreshMock.mockClear();
		storage = new AuthStorage(store, { refreshOAuthCredential: upstreamRefreshMock });
		await storage.credentials.reload();
		handle = startAuthBroker({ storage, bind: "127.0.0.1:0", bearerTokens: [TOKEN], disableRefresher: true });
	});

	afterEach(async () => {
		await handle?.close();
		storage?.close();
		store?.close();
		if (tempDir) await removeWithRetries(tempDir);
		handle = undefined;
		storage = undefined;
		store = undefined;
		tempDir = "";
	});

	test("returns 503 without credentials and leaves liveness static", async () => {
		if (!handle) throw new Error("expected broker from beforeEach");
		const readiness = await fetch(`${handle.url}/v1/readyz`);
		expect(readiness.status).toBe(503);
		expect(await readiness.json()).toEqual({ ok: false, reason: "no_usable_credentials" });

		const liveness = await fetch(`${handle.url}/v1/healthz`);
		expect(liveness.status).toBe(200);
		expect(await liveness.json()).toMatchObject({ ok: true });
	});

	test("returns 503 when every cached OAuth credential is expired", async () => {
		if (!storage || !handle) throw new Error("expected broker storage from beforeEach");
		await storage.credentials.upsert("anthropic", {
			type: "oauth",
			access: "expired-access",
			refresh: "refresh-token",
			expires: Date.now() - 1,
		});

		const response = await fetch(`${handle.url}/v1/readyz`);
		expect(response.status).toBe(503);
		expect(await response.json()).toEqual({ ok: false, reason: "no_usable_credentials" });
	});

	test("returns 200 when one cached OAuth credential is unexpired", async () => {
		if (!storage || !handle) throw new Error("expected broker storage from beforeEach");
		await storage.credentials.upsert("anthropic", {
			type: "oauth",
			access: "usable-access",
			refresh: "refresh-token",
			expires: Date.now() + 60_000,
		});

		const response = await fetch(`${handle.url}/v1/readyz`);
		expect(response.status).toBe(200);
		expect(await response.json()).toEqual({ ok: true });
	});

	test("treats an infinite OAuth expiry as long-lived", async () => {
		if (!store || !storage || !handle) throw new Error("expected broker storage from beforeEach");
		await storage.credentials.upsert("anthropic", {
			type: "oauth",
			access: "long-lived-access",
			refresh: "refresh-token",
			expires: Date.now() + 60_000,
		});
		const row = store.listAuthCredentials("anthropic")[0];
		if (!row) throw new Error("expected OAuth credential row");

		const writer = new Database(path.join(tempDir, "agent.db"));
		try {
			writer.run("UPDATE auth_credentials SET data = ? WHERE id = ?", [
				'{"access":"long-lived-access","refresh":"refresh-token","expires":1e999}',
				row.id,
			]);
		} finally {
			writer.close();
		}
		await storage.credentials.reload();

		const response = await fetch(`${handle.url}/v1/readyz`);
		expect(response.status).toBe(200);
		expect(await response.json()).toEqual({ ok: true });
	});

	test("returns 200 when a cached API key is non-empty", async () => {
		if (!storage || !handle) throw new Error("expected broker storage from beforeEach");
		await storage.credentials.upsert("anthropic", { type: "api_key", key: "usable-key" });

		const response = await fetch(`${handle.url}/v1/readyz`);
		expect(response.status).toBe(200);
		expect(await response.json()).toEqual({ ok: true });
	});

	test("returns 503 when a cached API key is empty", async () => {
		if (!storage || !handle) throw new Error("expected broker storage from beforeEach");
		await storage.credentials.upsert("anthropic", { type: "api_key", key: "" });

		const response = await fetch(`${handle.url}/v1/readyz`);
		expect(response.status).toBe(503);
		expect(await response.json()).toEqual({ ok: false, reason: "no_usable_credentials" });
	});

	test("does not reload credentials written directly to the store", async () => {
		if (!store || !storage || !handle) throw new Error("expected broker storage from beforeEach");
		const generation = storage.credentials.generation;
		await store.upsertAuthCredential("anthropic", { type: "api_key", key: "usable-key" });
		expect(storage.credentials.generation).toBe(generation);

		const response = await fetch(`${handle.url}/v1/readyz`);
		expect(response.status).toBe(503);
		expect(await response.json()).toEqual({ ok: false, reason: "no_usable_credentials" });
	});

	test("ignores malformed OAuth rows when another credential is usable", async () => {
		if (!store || !storage || !handle) throw new Error("expected broker storage from beforeEach");
		await storage.credentials.upsert("anthropic", {
			type: "oauth",
			access: "malformed-access",
			refresh: "malformed-refresh",
			expires: Date.now() + 60_000,
		});
		await storage.credentials.upsert("openai", {
			type: "oauth",
			access: "usable-access",
			refresh: "refresh-token",
			expires: Date.now() + 60_000,
		});
		const malformed = store.listAuthCredentials("anthropic")[0];
		if (!malformed) throw new Error("expected malformed OAuth row");

		const writer = new Database(path.join(tempDir, "agent.db"));
		try {
			writer.run("UPDATE auth_credentials SET data = ? WHERE id = ?", [
				JSON.stringify({ refresh: "malformed-refresh", expires: Date.now() + 60_000 }),
				malformed.id,
			]);
		} finally {
			writer.close();
		}
		await storage.credentials.reload();

		const response = await fetch(`${handle.url}/v1/readyz`);
		expect(response.status).toBe(200);
		expect(await response.json()).toEqual({ ok: true });
	});

	test("does not refresh expired credentials or call the upstream refresher", async () => {
		if (!storage || !handle) throw new Error("expected broker storage from beforeEach");
		await storage.credentials.upsert("anthropic", {
			type: "oauth",
			access: "expired-access",
			refresh: "refresh-token",
			expires: Date.now() - 1,
		});
		const refreshSpy = spyOn(storage.oauth, "refresh");

		const response = await fetch(`${handle.url}/v1/readyz`);
		expect(response.status).toBe(503);
		expect(refreshSpy).not.toHaveBeenCalled();
		expect(upstreamRefreshMock).not.toHaveBeenCalled();
		refreshSpy.mockRestore();
	});
});
