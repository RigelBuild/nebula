import { afterAll, afterEach, beforeAll, describe, expect, it, mock, spyOn } from "bun:test";
import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import { ModelRegistry } from "@oh-my-pi/pi-coding-agent/config/model-registry";
import { Settings } from "@oh-my-pi/pi-coding-agent/config/settings";
import {
	EXTENSION_HANDLER_TIMEOUT_MS,
	ExtensionRunner,
	testSetExtensionHandlerTimeoutMs,
} from "@oh-my-pi/pi-coding-agent/extensibility/extensions";
import { loadExtensions } from "@oh-my-pi/pi-coding-agent/extensibility/extensions/loader";
import type { ToolSession } from "@oh-my-pi/pi-coding-agent/tools";
import { BashTool } from "@oh-my-pi/pi-coding-agent/tools/bash";
import { AuthStorage } from "@oh-my-pi/pi-coding-agent/session/auth-storage";
import { SessionManager } from "@oh-my-pi/pi-coding-agent/session/session-manager";
import { Shell } from "@oh-my-pi/pi-natives";
import { TempDir } from "@oh-my-pi/pi-utils";

let sharedTempDir: TempDir;
let authStorage: AuthStorage;
let modelRegistry: ModelRegistry;

beforeAll(async () => {
	sharedTempDir = TempDir.createSync("@rig11916-shared-");
	authStorage = await AuthStorage.create(path.join(sharedTempDir.path(), "testauth.db"));
	modelRegistry = new ModelRegistry(authStorage);
});

afterAll(() => {
	authStorage.close();
	sharedTempDir.removeSync();
});

afterEach(() => {
	mock.restore();
	testSetExtensionHandlerTimeoutMs(EXTENSION_HANDLER_TIMEOUT_MS);
});

const secret = "rig11916-spill-secret-value-xyz";
// Enough raw output to overflow the inline budget, so the full text is spilled to the artifact.
const rawOutput = `LITELLM_API_KEY=${secret}\n` + "filler line of ordinary output\n".repeat(4000);

/**
 * Runs `env` through BashTool with an extension whose artifact_text handler body is `handlerBody`.
 * Returns the tool result, the spill file path, and whether the spill file still exists.
 */
async function runSpill(handlerBody: string) {
	spyOn(Shell.prototype, "run").mockImplementation(function (this: Shell, _options, onChunk) {
		onChunk?.(null, rawOutput);
		return Promise.resolve({ exitCode: 0, cancelled: false, timedOut: false, workingDir: process.cwd() });
	});

	const tempDir = TempDir.createSync("@rig11916-ext-");
	const artifactsDir = await fs.mkdtemp(path.join(os.tmpdir(), "bash-spill-redaction-"));
	try {
		const extPath = path.join(tempDir.path(), "redact.ts");
		await Bun.write(
			extPath,
			`export default function (pi) {
				pi.on("artifact_text", event => { ${handlerBody} });
			}`,
		);
		const loaded = await loadExtensions([extPath], tempDir.path());
		const runner = new ExtensionRunner(
			loaded.extensions,
			loaded.runtime,
			tempDir.path(),
			SessionManager.inMemory(),
			modelRegistry,
		);

		let nextId = 1;
		const session = {
			cwd: tempDir.path(),
			hasUI: false,
			skills: [],
			getSessionFile: () => null,
			settings: Settings.isolated({
				"async.enabled": false,
				"bash.autoBackground.enabled": false,
				"bash.autoBackground.thresholdMs": 60_000,
				"bashInterceptor.enabled": false,
				"astGrep.enabled": false,
				"astEdit.enabled": false,
				"grep.enabled": false,
				"glob.enabled": false,
			}),
			getClientBridge: () => undefined,
			allocateOutputArtifact: async () => {
				const id = String(nextId++);
				return { id, path: path.join(artifactsDir, `${id}.bash.log`) };
			},
			redactArtifactText: (text: string) => runner.redactArtifactText(text),
		} as unknown as ToolSession;

		const result = await new BashTool(session).execute("call-spill-redaction", { command: "env" });
		const spillPath = path.join(artifactsDir, "1.bash.log");
		const spillExists = await Bun.file(spillPath).exists();
		const written = spillExists ? await Bun.file(spillPath).text() : "";
		return { result, spillExists, written };
	} finally {
		await fs.rm(artifactsDir, { recursive: true, force: true });
		tempDir.removeSync();
	}
}

describe("BashTool full-output spill artifact redaction", () => {
	it("writes the spilled full output with the extension-registered secret redacted", async () => {
		const { result, written } = await runSpill(
			`return { text: event.text.split(${JSON.stringify(secret)}).join("[REDACTED:TOKEN]") };`,
		);

		expect(result.details?.meta?.truncation?.artifactId).toBe("1");
		expect(result.details?.meta?.truncation?.artifactError).toBeUndefined();
		expect(written).not.toContain(secret);
		expect(written).toContain("[REDACTED:TOKEN]");
	});

	it("withholds the spill when the artifact_text handler throws", async () => {
		const { result, spillExists, written } = await runSpill('throw new Error("redactor broke");');

		expect(spillExists).toBe(false);
		expect(written).not.toContain(secret);
		expect(result.details?.meta?.truncation?.artifactId).toBeUndefined();
		expect(result.details?.meta?.truncation?.artifactError).toBe("redact");
	});

	it("withholds the spill when the artifact_text handler times out", async () => {
		testSetExtensionHandlerTimeoutMs(50);
		const { result, spillExists, written } = await runSpill("return new Promise(() => {});");

		expect(spillExists).toBe(false);
		expect(written).not.toContain(secret);
		expect(result.details?.meta?.truncation?.artifactId).toBeUndefined();
		expect(result.details?.meta?.truncation?.artifactError).toBe("redact");
	});
});
