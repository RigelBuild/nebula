import { describe, expect, it } from "bun:test";
import {
	MEMORY_LOG_CHANGE_BYTES,
	MEMORY_LOG_HEARTBEAT_MS,
	type MemoryLogMark,
	shouldLogMemory,
} from "@oh-my-pi/pi-coding-agent/debug/memory-log";

const MB = 1024 * 1024;
const MARK: MemoryLogMark = { rss: 1_000 * MB, at: 0 };

describe("shouldLogMemory", () => {
	it("logs the first sample", () => {
		expect(shouldLogMemory(undefined, 1_000 * MB, 0)).toBe(true);
	});

	it("skips a sample while RSS moved under 256 MB and the heartbeat has not elapsed", () => {
		const rss = MARK.rss + MEMORY_LOG_CHANGE_BYTES - MB;
		expect(shouldLogMemory(MARK, rss, 30_000)).toBe(false);
	});

	it("logs once RSS moved by at least 256 MB, in either direction", () => {
		expect(shouldLogMemory(MARK, MARK.rss + MEMORY_LOG_CHANGE_BYTES, 30_000)).toBe(true);
		expect(shouldLogMemory(MARK, MARK.rss - MEMORY_LOG_CHANGE_BYTES, 30_000)).toBe(true);
	});

	it("logs after 10 minutes even when RSS has not changed", () => {
		expect(shouldLogMemory(MARK, MARK.rss, MEMORY_LOG_HEARTBEAT_MS - 1)).toBe(false);
		expect(shouldLogMemory(MARK, MARK.rss, MEMORY_LOG_HEARTBEAT_MS)).toBe(true);
	});
});
