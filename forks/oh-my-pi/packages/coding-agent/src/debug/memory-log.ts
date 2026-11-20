/** Periodic memory log line for long-lived omp processes (RIG-11152). */

import * as fs from "node:fs/promises";
import { logger } from "@oh-my-pi/pi-utils";
import { AgentRegistry } from "../registry/agent-registry";

export const MEMORY_LOG_INTERVAL_MS = 30_000;
export const MEMORY_LOG_CHANGE_BYTES = 256 * 1024 * 1024;
export const MEMORY_LOG_HEARTBEAT_MS = 10 * 60_000;

/** Parse the `RssAnon:` line of a `/proc/<pid>/status` text into bytes. */
export function parseRssAnon(statusText: string): number | undefined {
	const match = /^RssAnon:\s+(\d+)\s+kB\s*$/m.exec(statusText);
	if (!match) return undefined;
	return Number(match[1]) * 1024;
}

/** Read `RssAnon` for this process. Linux only; `undefined` elsewhere or on read failure. */
export async function readRssAnon(): Promise<number | undefined> {
	if (process.platform !== "linux") return undefined;
	try {
		return parseRssAnon(await fs.readFile("/proc/self/status", "utf8"));
	} catch {
		return undefined;
	}
}

/** The last logged sample, the basis for the change threshold. */
export interface MemoryLogMark {
	rss: number;
	at: number;
}

/**
 * Whether a sample should be logged: RSS moved by at least
 * {@link MEMORY_LOG_CHANGE_BYTES} since the last logged sample, or
 * {@link MEMORY_LOG_HEARTBEAT_MS} has passed since it. The first sample always logs.
 */
export function shouldLogMemory(mark: MemoryLogMark | undefined, rss: number, now: number): boolean {
	if (mark === undefined) return true;
	if (now - mark.at >= MEMORY_LOG_HEARTBEAT_MS) return true;
	return Math.abs(rss - mark.rss) >= MEMORY_LOG_CHANGE_BYTES;
}

/** Count of live (running or idle) subagents in the global registry. */
function liveSubagentCount(): number {
	return AgentRegistry.global()
		.list()
		.filter(ref => ref.kind === "sub" && (ref.status === "running" || ref.status === "idle")).length;
}

/**
 * Start the periodic memory sampler. The timer is unref'd, so it never keeps
 * the process alive. Returns a function that stops it.
 */
export function startMemoryLog(now: () => number = Date.now): () => void {
	let mark: MemoryLogMark | undefined;
	let stopped = false;

	const sample = async (): Promise<void> => {
		if (stopped) return;
		const usage = process.memoryUsage();
		const at = now();
		if (!shouldLogMemory(mark, usage.rss, at)) return;
		mark = { rss: usage.rss, at };
		const rssAnon = await readRssAnon();
		if (stopped) return;
		logger.info("memory", {
			rss: usage.rss,
			heapUsed: usage.heapUsed,
			external: usage.external,
			arrayBuffers: usage.arrayBuffers,
			rssAnon,
			subagents: liveSubagentCount(),
		});
	};

	const timer = setInterval(() => void sample(), MEMORY_LOG_INTERVAL_MS);
	timer.unref();
	void sample();
	return () => {
		stopped = true;
		clearInterval(timer);
	};
}
