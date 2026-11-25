import { afterEach, beforeEach, describe, expect, it, type Mock, spyOn } from "bun:test";
import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import { runAuthGatewayCommand, validateAuthGatewayConfig } from "@oh-my-pi/pi-coding-agent/cli/auth-gateway-cli";
import { resetSettingsForTest } from "@oh-my-pi/pi-coding-agent/config/settings";
import {
	__resetDirsFromEnvForTests,
	getProjectDir,
	removeWithRetries,
	setAgentDir,
	setProjectDir,
} from "@oh-my-pi/pi-utils";
import { restoreEnvValue } from "./helpers/settings-test-state";

// Captured at module load, before any test repoints the shared dirs resolver.
const PRISTINE_PROJECT_DIR = getProjectDir();

describe("omp auth-gateway validate", () => {
	const originalAgentDir = process.env.PI_CODING_AGENT_DIR;
	const originalOmpProfile = process.env.OMP_PROFILE;
	const originalPiProfile = process.env.PI_PROFILE;
	const originalConfigFiles = process.env.PI_CONFIG_FILES;
	const originalLimitClasses = process.env.OMP_AUTH_GATEWAY_API_KEY_LIMIT_MODEL_CLASSES;
	const originalExitCode = process.exitCode;

	let dir = "";
	let overlay = "";
	let listenSpy: Mock<typeof Bun.serve>;
	let fetchSpy: Mock<typeof fetch>;
	let stdoutSpy: Mock<typeof process.stdout.write>;
	let stderrSpy: Mock<typeof process.stderr.write>;

	beforeEach(async () => {
		dir = await fs.mkdtemp(path.join(os.tmpdir(), "gateway-validate-"));
		overlay = path.join(dir, "overlay.yml");
		await fs.mkdir(path.join(dir, "agent"));
		await fs.mkdir(path.join(dir, "project"));
		setAgentDir(path.join(dir, "agent"));
		setProjectDir(path.join(dir, "project"));
		process.env.PI_CONFIG_FILES = overlay;
		delete process.env.OMP_AUTH_GATEWAY_API_KEY_LIMIT_MODEL_CLASSES;
		process.exitCode = undefined;
		resetSettingsForTest();
		listenSpy = spyOn(Bun, "serve").mockImplementation(() => {
			throw new Error("validate must not listen");
		});
		fetchSpy = spyOn(globalThis, "fetch").mockImplementation(() =>
			Promise.reject(new Error("validate must not fetch")),
		);
		stdoutSpy = spyOn(process.stdout, "write").mockImplementation(() => true);
		stderrSpy = spyOn(process.stderr, "write").mockImplementation(() => true);
	});

	afterEach(async () => {
		listenSpy.mockRestore();
		fetchSpy.mockRestore();
		stdoutSpy.mockRestore();
		stderrSpy.mockRestore();
		process.exitCode = originalExitCode;
		restoreEnvValue("PI_CONFIG_FILES", originalConfigFiles);
		restoreEnvValue("OMP_AUTH_GATEWAY_API_KEY_LIMIT_MODEL_CLASSES", originalLimitClasses);
		restoreEnvValue("PI_CODING_AGENT_DIR", originalAgentDir);
		restoreEnvValue("OMP_PROFILE", originalOmpProfile);
		restoreEnvValue("PI_PROFILE", originalPiProfile);
		__resetDirsFromEnvForTests();
		setProjectDir(PRISTINE_PROJECT_DIR);
		resetSettingsForTest();
		await removeWithRetries(dir);
	});

	it("accepts a valid overlay with exit 0 and never listens or fetches", async () => {
		await Bun.write(overlay, "disassembler:\n  memoryMax: 4G\n");

		expect(await validateAuthGatewayConfig({})).toEqual([]);
		await runAuthGatewayCommand({ action: "validate", flags: {} });

		expect(process.exitCode ?? 0).toBe(0);
		expect(listenSpy).not.toHaveBeenCalled();
		expect(fetchSpy).not.toHaveBeenCalled();
	});

	it("exits non-zero and names the path of an invalid value", async () => {
		await Bun.write(overlay, "disassembler:\n  memoryMax: 12X\n");

		const problems = await validateAuthGatewayConfig({});
		expect(problems.join("\n")).toContain("disassembler.memoryMax");
		await runAuthGatewayCommand({ action: "validate", flags: {} });

		expect(process.exitCode).toBe(1);
		expect(stderrSpy).toHaveBeenCalled();
		expect(listenSpy).not.toHaveBeenCalled();
		expect(fetchSpy).not.toHaveBeenCalled();
	});

	it("exits non-zero and names the path of an unknown key", async () => {
		await Bun.write(overlay, "disassemblr:\n  memoryMax: 1G\n");

		expect(await validateAuthGatewayConfig({})).toEqual(['Unknown setting "disassemblr.memoryMax"']);
		await runAuthGatewayCommand({ action: "validate", flags: {} });

		expect(process.exitCode).toBe(1);
		expect(listenSpy).not.toHaveBeenCalled();
		expect(fetchSpy).not.toHaveBeenCalled();
	});
});
