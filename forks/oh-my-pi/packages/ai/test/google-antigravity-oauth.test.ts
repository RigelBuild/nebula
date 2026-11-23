import { afterEach, describe, expect, it, vi } from "bun:test";
import * as AIError from "../src/error";
import { AuthStorage, SqliteAuthCredentialStore } from "../src/auth-storage";
import { refreshOAuthToken } from "../src/registry/oauth";
import {
	ANTIGRAVITY_LOAD_CODE_ASSIST_METADATA,
	googleAntigravityProjectHook,
} from "../src/registry/oauth/google-antigravity";

const CLOUD_CODE_ASSIST_ENDPOINT = "https://daily-cloudcode-pa.googleapis.com";
const LOAD_CODE_ASSIST_URL = `${CLOUD_CODE_ASSIST_ENDPOINT}/v1internal:loadCodeAssist`;
const ONBOARD_USER_URL = `${CLOUD_CODE_ASSIST_ENDPOINT}/v1internal:onboardUser`;
const ONBOARD_OPERATION_URL = `${CLOUD_CODE_ASSIST_ENDPOINT}/v1internal/operations/onboard-123`;

function jsonResponse(body: unknown, status = 200): Response {
	return new Response(JSON.stringify(body), {
		status,
		headers: { "Content-Type": "application/json" },
	});
}

async function discoverAntigravityProject() {
	return googleAntigravityProjectHook(
		{ access: "access-token", refresh: "refresh-token", expires: 0 },
		{
			provider: "google-antigravity",
			phase: "login",
			raw: { refresh_token: "refresh-token" },
			fetch,
		},
	);
}

describe("Antigravity OAuth project discovery hook", () => {
	afterEach(() => {
		vi.restoreAllMocks();
	});

	it("loads and refreshes an existing account through the daily endpoint", async () => {
		const payload = {
			currentTier: { id: "free-tier" },
			paidTier: { id: "standard-tier" },
			allowedTiers: [{ id: "free-tier" }],
			cloudaicompanionProject: "project-123",
		};
		const fetchSpy = vi
			.spyOn(globalThis, "fetch")
			.mockImplementation(Object.assign(async () => jsonResponse(payload), { preconnect: fetch.preconnect }));

		const credentials = await discoverAntigravityProject();

		expect(credentials.projectId).toBe("project-123");
		expect(fetchSpy).toHaveBeenCalledTimes(2);
		for (const [url, init] of fetchSpy.mock.calls) {
			expect(url).toBe(LOAD_CODE_ASSIST_URL);
			expect(init?.method).toBe("POST");
			expect(init?.headers).toEqual({
				Authorization: "Bearer access-token",
				"Content-Type": "application/json",
				"User-Agent": expect.stringMatching(/^antigravity\/hub\//),
				"Accept-Encoding": "gzip",
			});
			expect(JSON.parse(String(init?.body))).toEqual({
				metadata: ANTIGRAVITY_LOAD_CODE_ASSIST_METADATA,
			});
		}
	});

	it("reloads with the returned project when paidTier is absent", async () => {
		const hydrated = {
			currentTier: { id: "free-tier" },
			paidTier: { id: "standard-tier" },
			allowedTiers: [{ id: "free-tier" }],
			cloudaicompanionProject: "project-123",
		};
		const responses = [
			{
				currentTier: { id: "free-tier" },
				allowedTiers: [{ id: "free-tier" }],
				cloudaicompanionProject: "project-123",
			},
			hydrated,
			hydrated,
		];
		const fetchSpy = vi.spyOn(globalThis, "fetch").mockImplementation(
			Object.assign(
				async () => {
					const payload = responses.shift();
					if (!payload) throw new Error("Unexpected Cloud Code Assist request");
					return jsonResponse(payload);
				},
				{ preconnect: fetch.preconnect },
			),
		);

		const credentials = await discoverAntigravityProject();

		expect(credentials.projectId).toBe("project-123");
		expect(fetchSpy).toHaveBeenCalledTimes(3);
		expect(JSON.parse(String(fetchSpy.mock.calls[0]?.[1]?.body))).toEqual({
			metadata: ANTIGRAVITY_LOAD_CODE_ASSIST_METADATA,
		});
		expect(JSON.parse(String(fetchSpy.mock.calls[1]?.[1]?.body))).toEqual({
			cloudaicompanionProject: "project-123",
			metadata: ANTIGRAVITY_LOAD_CODE_ASSIST_METADATA,
		});
		expect(JSON.parse(String(fetchSpy.mock.calls[2]?.[1]?.body))).toEqual({
			metadata: ANTIGRAVITY_LOAD_CODE_ASSIST_METADATA,
		});
	});

	it("onboards an account once with the native free-tier request", async () => {
		const responses = [
			{ allowedTiers: [{ id: "free-tier" }] },
			{
				name: "operations/onboard-123",
				done: true,
				response: {
					"@type": "type.googleapis.com/google.internal.cloud.code.v1internal.OnboardUserResponse",
					cloudaicompanionProject: "project-123",
				},
			},
			{
				currentTier: { id: "free-tier" },
				paidTier: { id: "standard-tier" },
				cloudaicompanionProject: "project-123",
			},
		];
		const fetchSpy = vi.spyOn(globalThis, "fetch").mockImplementation(
			Object.assign(
				async () => {
					const payload = responses.shift();
					if (!payload) throw new Error("Unexpected Cloud Code Assist request");
					return jsonResponse(payload);
				},
				{ preconnect: fetch.preconnect },
			),
		);

		const credentials = await discoverAntigravityProject();

		expect(credentials.projectId).toBe("project-123");
		expect(fetchSpy).toHaveBeenCalledTimes(3);
		expect(fetchSpy.mock.calls[1]?.[0]).toBe(ONBOARD_USER_URL);
		expect(fetchSpy.mock.calls[1]?.[1]?.method).toBe("POST");
		expect(JSON.parse(String(fetchSpy.mock.calls[1]?.[1]?.body))).toEqual({
			tierId: "free-tier",
			metadata: ANTIGRAVITY_LOAD_CODE_ASSIST_METADATA,
		});
		expect(fetchSpy.mock.calls[2]?.[0]).toBe(LOAD_CODE_ASSIST_URL);
	});

	it("polls a pending onboarding operation with GET after one second", async () => {
		const sleepSpy = vi.spyOn(Bun, "sleep").mockResolvedValue(undefined);
		const responses = [
			{ allowedTiers: [{ id: "free-tier" }] },
			{ name: "operations/onboard-123" },
			{
				name: "operations/onboard-123",
				done: true,
				response: {
					"@type": "type.googleapis.com/google.internal.cloud.code.v1internal.OnboardUserResponse",
					cloudaicompanionProject: "project-123",
				},
			},
			{
				currentTier: { id: "free-tier" },
				paidTier: { id: "standard-tier" },
				cloudaicompanionProject: "project-123",
			},
		];
		const fetchSpy = vi.spyOn(globalThis, "fetch").mockImplementation(
			Object.assign(
				async () => {
					const payload = responses.shift();
					if (!payload) throw new Error("Unexpected Cloud Code Assist request");
					return jsonResponse(payload);
				},
				{ preconnect: fetch.preconnect },
			),
		);

		const credentials = await discoverAntigravityProject();

		expect(credentials.projectId).toBe("project-123");
		expect(sleepSpy).toHaveBeenCalledTimes(1);
		expect(sleepSpy).toHaveBeenCalledWith(1_000);
		expect(fetchSpy).toHaveBeenCalledTimes(4);
		expect(fetchSpy.mock.calls[2]?.[0]).toBe(ONBOARD_OPERATION_URL);
		expect(fetchSpy.mock.calls[2]?.[1]?.method).toBe("GET");
		expect(fetchSpy.mock.calls[2]?.[1]?.headers).toEqual({
			Authorization: "Bearer access-token",
			"Content-Type": "application/json",
			"User-Agent": expect.stringMatching(/^antigravity\/hub\//),
			"Accept-Encoding": "gzip",
		});
		expect(fetchSpy.mock.calls[2]?.[1]?.body).toBeUndefined();
		expect(fetchSpy.mock.calls.filter(([url]) => url === ONBOARD_USER_URL)).toHaveLength(1);
	});

	it("surfaces native free-tier ineligibility without onboarding", async () => {
		const fetchSpy = vi.spyOn(globalThis, "fetch").mockResolvedValue(
			jsonResponse({
				ineligibleTiers: [
					{
						tierId: "free-tier",
						reasonMessage: "This account is not eligible for the free tier.",
						validationUrl: "https://example.test/validate",
					},
				],
			}),
		);

		await expect(discoverAntigravityProject()).rejects.toThrow("This account is not eligible for the free tier.");
		expect(fetchSpy).toHaveBeenCalledTimes(1);
		expect(fetchSpy.mock.calls[0]?.[0]).toBe(LOAD_CODE_ASSIST_URL);
	});

	it("rejects non-200 responses exactly like the native transport", async () => {
		const fetchSpy = vi
			.spyOn(globalThis, "fetch")
			.mockResolvedValue(new Response("created", { status: 201, statusText: "Created" }));

		await expect(discoverAntigravityProject()).rejects.toThrow("loadCodeAssist failed: 201 Created: created");
		expect(fetchSpy).toHaveBeenCalledTimes(1);
	});

	it("preserves stored identity when refresh userinfo is unavailable", async () => {
		const result = await googleAntigravityProjectHook(
			{ access: "fresh-access", refresh: "fresh-refresh", expires: Date.now() + 60_000 },
			{
				provider: "google-antigravity",
				phase: "refresh",
				raw: {},
				fetch,
				stored: {
					access: "old-access",
					refresh: "old-refresh",
					expires: 0,
					accountId: "account-123",
					email: "user@example.test",
					projectId: "project-123",
				},
			},
		);

		expect(result).toMatchObject({ accountId: "account-123", email: "user@example.test", projectId: "project-123" });
		expect(result.access).toBe("fresh-access");
	});

	it("rejects a refreshed credential whose account identity conflicts", async () => {
		await expect(
			googleAntigravityProjectHook(
				{
					access: "fresh-access",
					refresh: "fresh-refresh",
					expires: Date.now() + 60_000,
					accountId: "account-new",
				},
				{
					provider: "google-antigravity",
					phase: "refresh",
					raw: {},
					fetch,
					stored: {
						access: "old-access",
						refresh: "old-refresh",
						expires: 0,
						accountId: "account-old",
						projectId: "project-123",
					},
				},
			),
		).rejects.toThrow("account identity conflicts");
	});
});

describe("Antigravity paused OAuth refresh", () => {
	afterEach(() => {
		vi.restoreAllMocks();
	});

	it("refuses to refresh an existing credential without requesting a token", async () => {
		const fetchSpy = vi.spyOn(globalThis, "fetch");
		await expect(
			refreshOAuthToken("google-antigravity", {
				access: "old-access",
				refresh: "old-refresh",
				expires: 0,
				accountId: "account-123",
				email: "user@example.test",
				projectId: "project-123",
			}),
		).rejects.toBeInstanceOf(AIError.ProviderDisabledError);
		expect(fetchSpy).not.toHaveBeenCalled();
	});

	it("refuses to refresh a credential with an account identity", async () => {
		const fetchSpy = vi.spyOn(globalThis, "fetch");
		await expect(
			refreshOAuthToken("google-antigravity", {
				access: "old-access",
				refresh: "old-refresh",
				expires: 0,
				accountId: "account-123",
				projectId: "project-123",
			}),
		).rejects.toBeInstanceOf(AIError.ProviderDisabledError);
		expect(fetchSpy).not.toHaveBeenCalled();
	});

	it("refuses to refresh a credential before project validation", async () => {
		const fetchSpy = vi.spyOn(globalThis, "fetch");
		await expect(
			refreshOAuthToken("google-antigravity", {
				access: "old-access",
				refresh: "old-refresh",
				expires: 0,
				accountId: "account-123",
			}),
		).rejects.toBeInstanceOf(AIError.ProviderDisabledError);
		expect(fetchSpy).not.toHaveBeenCalled();
	});
});

describe("Antigravity paused OAuth login", () => {
	afterEach(() => {
		vi.restoreAllMocks();
	});

	it("refuses to start a login", async () => {
		const store = await SqliteAuthCredentialStore.open(":memory:");
		const storage = new AuthStorage(store);
		try {
			const fetchSpy = vi.spyOn(globalThis, "fetch");
			await expect(
				storage.oauth.login("google-antigravity", {
					onAuth: () => {},
					onPrompt: async () => "",
					onProgress: () => {},
				}),
			).rejects.toBeInstanceOf(AIError.ProviderDisabledError);
			expect(fetchSpy).not.toHaveBeenCalled();
		} finally {
			storage.close();
		}
	});
});

describe("Antigravity paused AuthStorage behavior", () => {
	afterEach(() => {
		vi.restoreAllMocks();
	});

	it("stores OAuth credentials but hides them from reads and key resolution", async () => {
		const store = await SqliteAuthCredentialStore.open(":memory:");
		const storage = new AuthStorage(store);
		const credential = {
			type: "oauth" as const,
			access: "stored-access",
			refresh: "stored-refresh",
			expires: Date.now() + 60_000,
			accountId: "account-123",
			projectId: "project-123",
		};
		try {
			await storage.credentials.set("google-antigravity", credential);

			expect(store.listAuthCredentials("google-antigravity")).toMatchObject([{ credential }]);
			expect(storage.credentials.get("google-antigravity")).toBeUndefined();
			expect(storage.credentials.list("google-antigravity")).toEqual([]);
			expect(storage.credentials.all()["google-antigravity"]).toBeUndefined();
			expect(storage.oauth.accounts("google-antigravity")).toEqual([]);
			expect(storage.credentials.getOAuth("google-antigravity")).toBeUndefined();
			expect(await storage.keys.get("google-antigravity")).toBeUndefined();
			await expect(storage.keys.getWithCredential("google-antigravity")).rejects.toBeInstanceOf(
				AIError.ProviderDisabledError,
			);
		} finally {
			storage.close();
		}
	});

	it("does not refresh expired stored credentials during key lookup", async () => {
		const refreshOAuthCredential = vi.fn(async () => ({
			access: "fresh-access",
			refresh: "fresh-refresh",
			expires: Date.now() + 60_000,
		}));
		const store = await SqliteAuthCredentialStore.open(":memory:");
		const storage = new AuthStorage(store, { refreshOAuthCredential });
		try {
			await storage.credentials.set("google-antigravity", {
				type: "oauth",
				access: "old-access",
				refresh: "old-refresh",
				expires: 0,
				accountId: "account-123",
				projectId: "project-123",
			});

			expect(await storage.keys.get("google-antigravity")).toBeUndefined();
			expect(refreshOAuthCredential).not.toHaveBeenCalled();
			expect(store.listAuthCredentials("google-antigravity")).toMatchObject([
				{ credential: { access: "old-access", accountId: "account-123" } },
			]);
		} finally {
			storage.close();
		}
	});

	it("does not force-refresh a credential hidden by the pause", async () => {
		const refreshOAuthCredential = vi.fn(async () => ({
			access: "fresh-access",
			refresh: "fresh-refresh",
			expires: Date.now() + 60_000,
			accountId: "account-other",
		}));
		const store = await SqliteAuthCredentialStore.open(":memory:");
		const storage = new AuthStorage(store, { refreshOAuthCredential });
		try {
			await storage.credentials.set("google-antigravity", {
				type: "oauth",
				access: "old-access",
				refresh: "old-refresh",
				expires: Date.now() + 60_000,
				accountId: "account-123",
				projectId: "project-123",
			});
			const row = store.listAuthCredentials("google-antigravity")[0];
			if (!row) throw new Error("missing stored credential");

			expect(storage.credentials.list("google-antigravity")).toEqual([]);
			await expect(storage.oauth.refresh(row.id)).rejects.toThrow(`No credential with id=${row.id}`);
			expect(refreshOAuthCredential).not.toHaveBeenCalled();
			expect(store.listAuthCredentials("google-antigravity")).toMatchObject([
				{ credential: { access: "old-access", accountId: "account-123" } },
			]);
		} finally {
			storage.close();
		}
	});

	it("does not call a delegated refresh when reading a paused provider", async () => {
		const refreshOAuthCredential = vi.fn(async () => ({
			access: "conflicting-access",
			refresh: "conflicting-refresh",
			expires: Date.now() + 60_000,
			accountId: "account-other",
		}));
		const store = await SqliteAuthCredentialStore.open(":memory:");
		const storage = new AuthStorage(store, { refreshOAuthCredential });
		try {
			await storage.credentials.set("google-antigravity", {
				type: "oauth",
				access: "old-access",
				refresh: "old-refresh",
				expires: 0,
				accountId: "account-123",
				projectId: "project-123",
			});

			expect(await storage.keys.get("google-antigravity")).toBeUndefined();
			expect(refreshOAuthCredential).not.toHaveBeenCalled();
			expect(store.listAuthCredentials("google-antigravity")).toMatchObject([
				{ credential: { access: "old-access", accountId: "account-123" } },
			]);
		} finally {
			storage.close();
		}
	});

	it("skips paused credentials during health checks before delegate refresh", async () => {
		const refreshOAuthCredential = vi.fn(async () => ({
			access: "conflicting-access",
			refresh: "conflicting-refresh",
			expires: Date.now() + 60_000,
			accountId: "account-other",
		}));
		const store = await SqliteAuthCredentialStore.open(":memory:");
		const storage = new AuthStorage(store, { refreshOAuthCredential });
		try {
			await storage.credentials.set("google-antigravity", {
				type: "oauth",
				access: "old-access",
				refresh: "old-refresh",
				expires: Date.now() - 60_000,
				accountId: "account-123",
				projectId: "project-123",
			});

			expect(await storage.health.check()).toEqual([]);
			expect(refreshOAuthCredential).not.toHaveBeenCalled();
			expect(store.listAuthCredentials("google-antigravity")).toMatchObject([
				{ credential: { access: "old-access", accountId: "account-123" } },
			]);
		} finally {
			storage.close();
		}
	});

	it("continues to skip credentials changed in storage by another process", async () => {
		const refreshOAuthCredential = vi.fn(async () => {
			throw new Error("unexpected refresh");
		});
		const store = await SqliteAuthCredentialStore.open(":memory:");
		const storage = new AuthStorage(store, { refreshOAuthCredential });
		try {
			await storage.credentials.set("google-antigravity", {
				type: "oauth",
				access: "old-access",
				refresh: "old-refresh",
				expires: Date.now() - 60_000,
				accountId: "account-123",
				projectId: "project-123",
			});
			const listAuthCredentials = store.listAuthCredentials.bind(store);
			let peerRotated = false;
			vi.spyOn(store, "listAuthCredentials").mockImplementation(provider => {
				if (provider === "google-antigravity" && !peerRotated) {
					peerRotated = true;
					const row = listAuthCredentials(provider)[0];
					if (!row) throw new Error("missing stored credential");
					store.updateAuthCredential(row.id, {
						type: "oauth",
						access: "peer-access",
						refresh: "peer-refresh",
						expires: Date.now() + 120_000,
						accountId: "account-other",
						projectId: "project-123",
					});
				}
				return listAuthCredentials(provider);
			});

			expect(await storage.health.check()).toEqual([]);
			expect(refreshOAuthCredential).not.toHaveBeenCalled();
			expect(store.listAuthCredentials("google-antigravity")[0]?.credential).toMatchObject({
				type: "oauth",
				access: "peer-access",
				accountId: "account-other",
			});
		} finally {
			storage.close();
		}
	});

	it("skips paused expired credentials during health checks without lease support", async () => {
		const refreshOAuthCredential = vi.fn(async () => ({
			access: "conflicting-access",
			refresh: "conflicting-refresh",
			expires: Date.now() + 60_000,
			accountId: "account-other",
		}));
		const store = await SqliteAuthCredentialStore.open(":memory:");
		const leaseMethods: Record<string, true> = {
			tryAcquireCredentialRefreshLease: true,
			getCredentialRefreshLeaseExpiresAt: true,
			releaseCredentialRefreshLease: true,
			renewCredentialRefreshLease: true,
		};
		const storeWithoutLeases = new Proxy(store, {
			get(target, property) {
				if (typeof property === "string" && leaseMethods[property] === true) return undefined;
				const value = Reflect.get(target, property, target);
				return typeof value === "function" ? value.bind(target) : value;
			},
		});
		const storage = new AuthStorage(storeWithoutLeases, { refreshOAuthCredential });
		try {
			await storage.credentials.set("google-antigravity", {
				type: "oauth",
				access: "old-access",
				refresh: "old-refresh",
				expires: Date.now() - 60_000,
				accountId: "account-123",
				projectId: "project-123",
			});

			expect(await storage.health.check()).toEqual([]);
			expect(refreshOAuthCredential).not.toHaveBeenCalled();
			expect(store.listAuthCredentials("google-antigravity")).toMatchObject([
				{ credential: { access: "old-access", accountId: "account-123" } },
			]);
		} finally {
			storage.close();
		}
	});
});
