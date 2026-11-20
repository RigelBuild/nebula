import { describe, expect, it, spyOn } from "bun:test";
import { startMemoryLog } from "@oh-my-pi/pi-coding-agent/debug/memory-log";
import { logger } from "@oh-my-pi/pi-utils";

// The startup sample awaits a /proc read, so poll on a later I/O turn until
// logger.info has been called, or give up after 2 s.
async function settle(info: { mock: { calls: unknown[][] } }): Promise<void> {
	const deadline = Date.now() + 2_000;
	while (info.mock.calls.length === 0 && Date.now() < deadline) {
		const { promise, resolve } = Promise.withResolvers<void>();
		setImmediate(resolve);
		await promise;
	}
}

describe("startMemoryLog", () => {
	it("logs a startup sample", async () => {
		const info = spyOn(logger, "info").mockImplementation(() => {});
		try {
			const stop = startMemoryLog();
			await settle(info);
			stop();
			expect(info).toHaveBeenCalledTimes(1);
			expect(info.mock.calls[0]?.[0]).toBe("memory");
		} finally {
			info.mockRestore();
		}
	});

	it("does not log when stopped before the sample resolves", async () => {
		const info = spyOn(logger, "info").mockImplementation(() => {});
		try {
			startMemoryLog()();
			const { promise, resolve } = Promise.withResolvers<void>();
			setTimeout(resolve, 200);
			await promise;
			expect(info.mock.calls.map(call => call[0])).not.toContain("memory");
		} finally {
			info.mockRestore();
		}
	});
});
