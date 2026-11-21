import { beforeEach, describe, expect, it } from "bun:test";
import { RunTrace, Shell } from "@oh-my-pi/pi-natives";
import { isShellTrace, type ShellTrace, type ShellVizPresentation } from "@oh-my-pi/pi-wire";
import {
	attachShellTrace,
	clearRestoredVisualizations,
	decodeShellPresentation,
	defaultShellVizPreferences,
	readShellPresentation,
	restoredVisualizationCount,
	shellVisualization,
} from "../src/tools/shell-viz";
import { renderShellVisualization } from "../src/tools/shell-viz-render";
import { getThemeByName } from "../src/theme";

function snapshot(trace: RunTrace): ShellTrace {
	const value: unknown = JSON.parse(trace.snapshot());
	if (!isShellTrace(value)) throw new Error("Expected native execution trace");
	return value;
}
function fixture(): ShellVizPresentation {
	return {
		preferences: { ...defaultShellVizPreferences(), selectedNode: "2:0:17", expandedNodes: ["2:0:17"] },
		rows: [
			{
				id: "command",
				depth: 0,
				node: "2:0:17",
				range: [0, 9],
				state: "ok",
				segments: [{ text: "print(1)", node: "2:0:17", state: "ok", executedAtNs: 4, executionSeq: 8 }],
				effects: [],
				annotations: [{ name: "x", value: "1", node: "2:0:17", setterSeq: 7 }],
			},
		],
		original: [],
		output: "1\n",
		executing: false,
		watches: [{ name: "x", value: "1", node: "2:0:17", setterSeq: 7 }],
		timeline: { length: 8, cursor: 8, elapsedNs: 4, durationNs: 4 },
	};
}

describe("native shell presentation boundary", () => {
	beforeEach(() => {
		clearRestoredVisualizations();
	});

	it("rejects malformed DTOs and numeric node identities without compatibility coercion", () => {
		for (const value of [
			{ ...fixture(), rows: [{ ...fixture().rows[0], node: 17 }] },
			{ ...fixture(), rows: [{ ...fixture().rows[0], segments: [{ text: "x", node: "17" }] }] },
			{ ...fixture(), rows: [{ ...fixture().rows[0], segments: [{ text: "x", executionNode: 17 }] }] },
			{ ...fixture(), watches: [{ name: "x", value: "1", node: "2:0:-1" }] },
			{ ...fixture(), preferences: { ...fixture().preferences, expandedNodes: [17] } },
			{ ...fixture(), timeline: { length: 8, cursor: "8", elapsedNs: 4, durationNs: 4 } },
		])
			expect(() => decodeShellPresentation(JSON.stringify(value))).toThrow("invalid visualization presentation");
	});

	it("keeps an unexecuted pending trace uncompiled, including invalid future shell text", () => {
		const trace = new RunTrace("printf first\nif then invalid");
		const before = snapshot(trace);
		readShellPresentation(trace, defaultShellVizPreferences());
		expect(snapshot(trace).units).toEqual([]);
		expect(snapshot(trace).seq).toBe(before.seq);
	});

	it("marks a failed command once, not once per source argument", async () => {
		const command = "false first second\n:";
		const trace = new RunTrace(command);
		const result = await new Shell().run({ command, trace });
		expect(result.exitCode).toBe(0);
		const presentation = readShellPresentation(trace, defaultShellVizPreferences(), snapshot(trace));
		const selectedTheme = await getThemeByName("dark");
		if (!selectedTheme) throw new Error("Missing dark theme");
		const output = Bun.stripANSI(renderShellVisualization(presentation, selectedTheme, 100).join("\n"));
		expect(output.match(/×/g)).toHaveLength(1);
		expect(output).toContain("false first second");
	});

	it("uses the sink only live and replays real captured output from live or stored traces", async () => {
		const command = "printf 'native-boundary\\n'";
		const trace = new RunTrace(command);
		const shell = new Shell();
		await shell.run({ command, trace });
		const live = snapshot(trace);
		attachShellTrace(live, trace);
		const preferences = defaultShellVizPreferences();
		const attached = shellVisualization(live);
		if (!attached) throw new Error("Expected the attached live trace");
		expect(readShellPresentation(attached, preferences, live, "sink notice").output).toBe("sink notice");
		const replay = { ...preferences, cursor: live.history?.length ?? 0 };
		expect(readShellPresentation(trace, replay, live, "sink notice").output).toBe("native-boundary\n");
		const stored = snapshot(trace);
		const restored = shellVisualization(stored);
		if (!restored) throw new Error("Expected the stored trace to restore");
		expect(readShellPresentation(restored, replay, stored).output).toBe("native-boundary\n");
	});

	it("bounds the restored visualization cache and rebuilds an evicted entry from JSON", async () => {
		const snapshots: ShellTrace[] = [];
		for (let i = 0; i < 20; i++) {
			const command = `printf 'bounded-${i}\\n'`;
			const trace = new RunTrace(command);
			await new Shell().run({ command, trace });
			snapshots.push(snapshot(trace));
		}
		const replayOf = (stored: ShellTrace) => ({
			...defaultShellVizPreferences(),
			cursor: stored.history?.length ?? 0,
		});
		for (const stored of snapshots) {
			const restored = shellVisualization(stored);
			if (!restored) throw new Error("Expected the stored trace to restore");
			expect(restoredVisualizationCount()).toBeLessThanOrEqual(16);
		}
		expect(restoredVisualizationCount()).toBe(16);
		// The first snapshot was evicted; restoring it again still renders its output.
		const first = snapshots[0];
		if (!first) throw new Error("Expected a first snapshot");
		const again = shellVisualization(first);
		if (!again) throw new Error("Expected the evicted trace to restore from JSON");
		expect(readShellPresentation(again, replayOf(first), first).output).toBe("bounded-0\n");
		expect(restoredVisualizationCount()).toBe(16);
	});

	it("keeps the recently restored entry and rebuilds the evicted one", async () => {
		const snapshots: ShellTrace[] = [];
		for (let i = 0; i < 17; i++) {
			const command = `printf 'lru-${i}\\n'`;
			const trace = new RunTrace(command);
			await new Shell().run({ command, trace });
			snapshots.push(snapshot(trace));
		}
		const first = snapshots[0];
		const second = snapshots[1];
		const last = snapshots[16];
		if (!first || !second || !last) throw new Error("Expected 17 snapshots");
		const restoredFirst = shellVisualization(first);
		const restoredSecond = shellVisualization(second);
		if (!restoredFirst || !restoredSecond) throw new Error("Expected the first two traces to restore");
		for (const stored of snapshots.slice(2, 16)) expect(shellVisualization(stored)).toBeDefined();
		expect(restoredVisualizationCount()).toBe(16);
		// Touching the oldest entry makes the second one the least recently used.
		expect(shellVisualization(first)).toBe(restoredFirst);
		expect(shellVisualization(last)).toBeDefined();
		expect(restoredVisualizationCount()).toBe(16);
		// The touched entry survived; the untouched one was evicted and rebuilds as a new object.
		expect(shellVisualization(first)).toBe(restoredFirst);
		const rebuilt = shellVisualization(second);
		if (!rebuilt) throw new Error("Expected the evicted trace to restore from JSON");
		expect(rebuilt).not.toBe(restoredSecond);
		const replay = { ...defaultShellVizPreferences(), cursor: second.history?.length ?? 0 };
		expect(readShellPresentation(rebuilt, replay, second).output).toBe("lru-1\n");
	});
});
