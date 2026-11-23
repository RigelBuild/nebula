import { beforeEach, describe, expect, it } from "bun:test";
import type { DaemonSnapshot } from "@oh-my-pi/pi-tui/tools/daemon";
import { Settings } from "../../src/config/settings";
import { startHost } from "../../src/disasm/client";
import { resetMemoryScopeForTest } from "../../src/disasm/memory-scope";
import type { DatabaseLocation } from "../../src/disasm/store";
import type { DaemonBrokerClient } from "../../src/launch/client";
import type { ToolSession } from "../../src/tools";

describe("disassembler host startup", () => {
	beforeEach(() => resetMemoryScopeForTest());

	it("preserves readiness from the broker snapshot after a host exits", async () => {
		const name = "omp.ida.test";
		const daemon: DaemonSnapshot = {
			name,
			id: "test",
			state: "exited",
			createdAt: 1,
			startedAt: 2,
			readyAt: 3,
			exitCode: 1,
			restartCount: 0,
			outputBytes: 0,
			persist: false,
			detached: false,
		};
		let starts = 0;
		let probes = 0;
		const broker: DaemonBrokerClient = {
			projectDir: "/tmp",
			onCompletion: () => () => {},
			close: () => {},
			request: async operation => {
				if (operation.op !== "start") throw new Error("Unexpected broker request");
				starts++;
				return { op: "start", daemon, readyTimedOut: false };
			},
		};
		const loc: DatabaseLocation = {
			id: "test",
			dir: "/tmp",
			sourcePath: "/tmp/test.i64",
			stagedPath: "/tmp/test.i64",
			kind: "inplace",
			openPath: "/tmp/test.i64",
			isNew: false,
			lockTarget: "/tmp/test.i64",
		};
		// Only settings are read because runtime resolution is injected.
		const session = { settings: Settings.isolated({}) } as unknown as ToolSession;
		await expect(
			startHost(session, broker, "ida", loc, name, {
				resolveRuntime: async () => ({ pythonPath: "python", env: {} }),
				platform: "linux",
				systemdRun: "/usr/bin/systemd-run",
				probe: async () => {
					probes++;
					return false;
				},
			}),
		).rejects.toThrow("exited during startup");
		expect(starts).toBe(1);
		expect(probes).toBe(0);
	});
});
