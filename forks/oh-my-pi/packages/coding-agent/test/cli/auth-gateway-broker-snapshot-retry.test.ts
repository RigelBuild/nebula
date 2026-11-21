import { afterEach, describe, expect, test, vi } from "bun:test";
import { AuthBrokerClient, AuthBrokerError, type SnapshotResponse } from "@oh-my-pi/pi-ai/auth-broker";
import { logger } from "@oh-my-pi/pi-utils";
import { fetchBrokerSnapshot } from "../../src/cli/auth-gateway-cli";

const PROVIDER = "unit-gateway-broker-retry";
const SNAPSHOT: SnapshotResponse = {
	generation: 7,
	generatedAt: 1,
	serverNowMs: 1,
	refresher: { enabled: false, intervalMs: 60_000, skewMs: 300_000, nextSweepInMs: Number.MAX_SAFE_INTEGER },
	credentials: [
		{
			id: 1,
			provider: PROVIDER,
			credential: { type: "api_key", key: "unit-api-key" },
			identityKey: null,
			rotatesInMs: null,
		},
	],
};

function snapshotReply(): Response {
	return new Response(JSON.stringify(SNAPSHOT), { status: 200, headers: { "content-type": "application/json" } });
}

/**
 * `fetch` rejects with a `TypeError` that carries the errno `code` itself, as Bun 1.4.4 reports it.
 * `AuthBrokerClient` wraps it once in `AuthBrokerError{cause}`, so the code sits at depth 1.
 */
function socketFailure(code: string): TypeError {
	return Object.assign(new TypeError(`fetch failed: ${code} 127.0.0.1:4002`), { code });
}

/** A client whose n-th broker request gets `reply(n)`. `maxRetries: 0` keeps request counts exact. */
function brokerReplying(reply: (call: number) => Response | Error): { client: AuthBrokerClient; calls: () => number } {
	let calls = 0;
	const fetchImpl = Object.assign(
		async (_input: string | URL | Request, _init?: RequestInit): Promise<Response> => {
			calls += 1;
			const outcome = reply(calls);
			if (outcome instanceof Error) throw outcome;
			return outcome;
		},
		{ preconnect: fetch.preconnect },
	);
	const client = new AuthBrokerClient({ url: "http://broker.invalid", token: "unused", maxRetries: 0, fetchImpl });
	return { client, calls: () => calls };
}

/** Virtual clock: `sleep` advances `now` instead of waiting, and records each wait. */
function virtualClock() {
	let elapsedMs = 0;
	const sleeps: number[] = [];
	return {
		now: () => elapsedMs,
		sleep: async (ms: number): Promise<void> => {
			sleeps.push(ms);
			elapsedMs += ms;
		},
		sleeps,
	};
}

async function thrownBy(promise: Promise<unknown>): Promise<unknown> {
	try {
		await promise;
	} catch (error) {
		return error;
	}
	return undefined;
}

describe("auth-gateway broker snapshot retry", () => {
	afterEach(() => {
		vi.restoreAllMocks();
	});

	test("retries refused connections until the broker listens", async () => {
		const { client, calls } = brokerReplying(call => (call <= 2 ? socketFailure("ECONNREFUSED") : snapshotReply()));
		const clock = virtualClock();
		const warn = vi.spyOn(logger, "warn").mockImplementation(() => {});

		const snapshot = await fetchBrokerSnapshot(client, clock);

		expect(snapshot).toEqual(SNAPSHOT);
		expect(calls()).toBe(3);
		expect(clock.sleeps).toEqual([250, 500]);
		expect(warn).toHaveBeenCalledTimes(2);
	});

	test("gives up with the last error once the 30s deadline passes", async () => {
		const { client, calls } = brokerReplying(() => socketFailure("ECONNREFUSED"));
		const clock = virtualClock();
		const warn = vi.spyOn(logger, "warn").mockImplementation(() => {});

		const thrown = await thrownBy(fetchBrokerSnapshot(client, clock));

		expect(thrown).toBeInstanceOf(AuthBrokerError);
		expect(thrown instanceof AuthBrokerError ? thrown.cause : undefined).toBeInstanceOf(TypeError);
		// Backoff doubles 250ms → 4s cap; the last wait is trimmed so the total is exactly 30s.
		expect(clock.sleeps).toEqual([250, 500, 1000, 2000, 4000, 4000, 4000, 4000, 4000, 4000, 2250]);
		expect(clock.sleeps.reduce((total, ms) => total + ms, 0)).toBe(30_000);
		expect(calls()).toBe(clock.sleeps.length + 1);
		expect(warn).toHaveBeenCalledTimes(clock.sleeps.length);
	});

	test("fails at once on a non-200 broker response", async () => {
		const { client, calls } = brokerReplying(() => new Response("broker unavailable", { status: 503 }));
		const clock = virtualClock();
		const warn = vi.spyOn(logger, "warn").mockImplementation(() => {});

		const thrown = await thrownBy(fetchBrokerSnapshot(client, clock));

		expect(thrown).toBeInstanceOf(AuthBrokerError);
		expect(thrown).toMatchObject({ status: 503 });
		expect(calls()).toBe(1);
		expect(clock.sleeps).toEqual([]);
		expect(warn).not.toHaveBeenCalled();
	});

	test("retries connection resets too", async () => {
		const { client, calls } = brokerReplying(call => (call === 1 ? socketFailure("ECONNRESET") : snapshotReply()));
		const clock = virtualClock();
		vi.spyOn(logger, "warn").mockImplementation(() => {});

		const snapshot = await fetchBrokerSnapshot(client, clock);

		expect(snapshot).toEqual(SNAPSHOT);
		expect(calls()).toBe(2);
		expect(clock.sleeps).toEqual([250]);
	});

	test("fails at once on any other transport error", async () => {
		const { client, calls } = brokerReplying(() => socketFailure("ENOTFOUND"));
		const clock = virtualClock();
		const warn = vi.spyOn(logger, "warn").mockImplementation(() => {});

		const thrown = await thrownBy(fetchBrokerSnapshot(client, clock));

		expect(thrown).toBeInstanceOf(AuthBrokerError);
		expect(calls()).toBe(1);
		expect(clock.sleeps).toEqual([]);
		expect(warn).not.toHaveBeenCalled();
	});
});
