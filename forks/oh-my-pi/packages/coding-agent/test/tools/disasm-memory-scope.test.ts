import { beforeEach, describe, expect, it } from "bun:test";
import { cfgDisasmMemoryMax } from "../../src/disasm/settings";
import {
	resetMemoryScopeForTest,
	startWithMemoryScopeFallback,
	withMemoryScope,
	type HostStartupResult,
} from "../../src/disasm/memory-scope";

describe("disassembler host memory scope", () => {
	const command = ["bun", "run", "worker.ts", "--flag"];
	beforeEach(() => resetMemoryScopeForTest());

	it("prefixes the host command with a Linux systemd memory scope", () => {
		expect(withMemoryScope(command, "6G", "/usr/bin/systemd-run", "linux")).toEqual([
			"/usr/bin/systemd-run",
			"--user",
			"--scope",
			"--slice-inherit",
			"--quiet",
			"--collect",
			"-p",
			"MemoryMax=6G",
			"-p",
			"MemorySwapMax=0",
			"--",
			"bun",
			"run",
			"worker.ts",
			"--flag",
		]);
	});

	it("leaves argv unchanged when the limit is empty", () => {
		expect(withMemoryScope(command, "", "/usr/bin/systemd-run", "linux")).toEqual(command);
	});

	it("leaves argv unchanged when systemd-run is missing", () => {
		expect(withMemoryScope(command, "6G", undefined, "linux")).toEqual(command);
	});

	it("leaves argv unchanged outside Linux", () => {
		expect(withMemoryScope(command, "6G", "/usr/bin/systemd-run", "darwin")).toEqual(command);
	});

	it("keeps the scope after a host exits after readiness", async () => {
		const calls: string[][] = [];
		let probes = 0;
		const result = await startWithMemoryScopeFallback(
			command,
			"6G",
			"/usr/bin/systemd-run",
			"linux",
			async cmd => {
				calls.push([...cmd]);
				return { state: "exited", reason: "crash", readyAt: 1 };
			},
			() => {
				throw new Error("Scope disabled after readiness");
			},
			async () => {
				probes++;
				return false;
			},
		);
		expect(result).toEqual({ state: "exited", reason: "crash", readyAt: 1 });
		expect(calls).toHaveLength(1);
		expect(calls[0]?.[0]).toBe("/usr/bin/systemd-run");
		expect(probes).toBe(0);
	});

	it("keeps the scope when a pre-ready exit has a passing probe", async () => {
		const calls: string[][] = [];
		let probes = 0;
		const result = await startWithMemoryScopeFallback(
			command,
			"6G",
			"/usr/bin/systemd-run",
			"linux",
			async cmd => {
				calls.push([...cmd]);
				return { state: "exited", reason: "host failed" };
			},
			() => {
				throw new Error("Scope disabled despite passing probe");
			},
			async () => {
				probes++;
				return true;
			},
		);
		expect(result).toEqual({ state: "exited", reason: "host failed" });
		expect(calls).toHaveLength(1);
		expect(calls[0]?.[0]).toBe("/usr/bin/systemd-run");
		expect(probes).toBe(1);
	});

	it("shares and caches a passing scope probe", async () => {
		let probes = 0;
		const start = async (): Promise<HostStartupResult<string>> => ({ state: "exited", reason: "host failed" });
		const probe = async (systemdRun: string, memoryMax: string) => {
			expect([systemdRun, memoryMax]).toEqual(["/usr/bin/systemd-run", "6G"]);
			probes++;
			return true;
		};
		const run = () =>
			startWithMemoryScopeFallback(command, "6G", "/usr/bin/systemd-run", "linux", start, () => {}, probe);
		await Promise.all([run(), run()]);
		await run();
		expect(probes).toBe(1);
	});

	it("retries a scoped startup exit without the scope", async () => {
		const calls: string[][] = [];
		const started: HostStartupResult<string>[] = [
			{ state: "exited", reason: "code 1" },
			{ state: "started", value: "ready" },
			{ state: "started", value: "ready again" },
		];
		const skipped: string[] = [];
		const result = await startWithMemoryScopeFallback(
			command,
			"6G",
			"/usr/bin/systemd-run",
			"linux",
			async cmd => {
				calls.push([...cmd]);
				const next = started.shift();
				if (!next) throw new Error("Unexpected extra host start");
				return next;
			},
			reason => skipped.push(reason),
			async () => false,
		);

		expect(calls).toEqual([
			[
				"/usr/bin/systemd-run",
				"--user",
				"--scope",
				"--slice-inherit",
				"--quiet",
				"--collect",
				"-p",
				"MemoryMax=6G",
				"-p",
				"MemorySwapMax=0",
				"--",
				...command,
			],
			command,
		]);
		expect(result).toEqual({ state: "started", value: "ready" });
		const subsequentResult = await startWithMemoryScopeFallback(
			command,
			"6G",
			"/usr/bin/systemd-run",
			"linux",
			async cmd => {
				calls.push([...cmd]);
				const next = started.shift();
				if (!next) throw new Error("Unexpected extra host start");
				return next;
			},
			reason => skipped.push(reason),
		);
		expect(calls[2]).toEqual(command);
		expect(subsequentResult).toEqual({ state: "started", value: "ready again" });
		expect(skipped).toEqual(["startup-failure", "previous-failure"]);
	});

	it("disassembler.memoryMax accepts systemd memory values only", () => {
		const validate = cfgDisasmMemoryMax.definition.validate;
		if (!validate) throw new Error("disassembler.memoryMax has no validation hook");

		for (const value of [
			"",
			"infinity",
			"100",
			"100K",
			"100M",
			"100G",
			"100T",
			"75%",
			"1.5G",
			"1.5%",
			"6P",
			"6E",
			"0.01%",
			"100%",
		]) {
			expect(() => validate(value)).not.toThrow();
		}
		for (const value of [
			"Infinity",
			"100B",
			"bogus",
			" 100M",
			"-1M",
			"0",
			"0%",
			"0.4",
			"1.0001%",
			"99999999999E",
			"200%",
			"0x10",
			"-1",
			"6g",
		]) {
			expect(() => validate(value)).toThrow();
		}
	});
});
