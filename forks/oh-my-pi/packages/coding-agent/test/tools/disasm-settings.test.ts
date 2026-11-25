import { describe, expect, it } from "bun:test";
import * as fs from "node:fs";
import * as path from "node:path";
import { Settings } from "@oh-my-pi/pi-coding-agent/config/settings";
import { TempDir } from "@oh-my-pi/pi-utils";
import { cfgDisasmMemoryMax } from "../../src/disasm/settings";

async function loadSettingsFromConfig(config: string): Promise<Settings> {
	const tempDir = TempDir.createSync("@omp-disasm-settings-");
	const agentDir = tempDir.join("agent");
	const cwd = tempDir.join("project");
	fs.mkdirSync(agentDir, { recursive: true });
	fs.mkdirSync(cwd, { recursive: true });
	await Bun.write(path.join(agentDir, "config.yml"), config);
	try {
		return await Settings.loadReadOnly({ agentDir, cwd });
	} finally {
		tempDir.removeSync();
	}
}

describe("disassembler.memoryMax settings", () => {
	it("loads a settings file without memoryMax and uses the default", async () => {
		const settings = await loadSettingsFromConfig("disassembler:\n  backend: auto\n");

		expect(cfgDisasmMemoryMax.get(settings)).toBe("6G");
	});

	it("rejects a non-string configured memoryMax", async () => {
		await expect(loadSettingsFromConfig("disassembler:\n  memoryMax: 5\n")).rejects.toThrow(
			"disassembler.memoryMax must be a systemd memory value",
		);
	});
});
