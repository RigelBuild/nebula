import { afterAll, afterEach, beforeAll, describe, expect, it, mock } from "bun:test";
import * as fs from "node:fs/promises";
import * as path from "node:path";
import type { AgentToolContext, AgentToolResult } from "@oh-my-pi/pi-agent-core";
import { ModelRegistry } from "@oh-my-pi/pi-coding-agent/config/model-registry";
import { Settings } from "@oh-my-pi/pi-coding-agent/config/settings";
import {
	EXTENSION_HANDLER_TIMEOUT_MS,
	ExtensionRunner,
	testSetExtensionHandlerTimeoutMs,
} from "@oh-my-pi/pi-coding-agent/extensibility/extensions";
import { loadExtensions } from "@oh-my-pi/pi-coding-agent/extensibility/extensions/loader";
import { ArtifactManager } from "@oh-my-pi/pi-coding-agent/session/artifacts";
import { AuthStorage } from "@oh-my-pi/pi-coding-agent/session/auth-storage";
import { SessionManager } from "@oh-my-pi/pi-coding-agent/session/session-manager";
import { postProcessToolResult } from "@oh-my-pi/pi-coding-agent/tools/output-meta";
import { TempDir } from "@oh-my-pi/pi-utils";

let sharedTempDir: TempDir;
let authStorage: AuthStorage;
let modelRegistry: ModelRegistry;

beforeAll(async () => {
	sharedTempDir = TempDir.createSync("@rig11916-mcp-shared-");
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

const toolName = "mcp__test_server_tool";
const secret = "rig11916-mcp-secret-value-xyz";
// Enough raw output to overflow the spill threshold, so the full text is written to an artifact.
const rawOutput = `API_KEY=${secret}\n` + "filler line of ordinary output\n".repeat(4000);

/**
 * Runs an oversized MCP-named tool result through the spill with an extension whose artifact_text
 * handler body is `handlerBody`. Returns the post-processed result, the artifact file on disk (if
 * any), and its contents.
 */
async function runSpill(handlerBody: string) {
	const tempDir = TempDir.createSync("@rig11916-mcp-ext-");
	const artifactsDir = path.join(tempDir.path(), "artifacts");
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

		const sessionManager = SessionManager.inMemory();
		sessionManager.adoptArtifactManager(new ArtifactManager(artifactsDir));

		const context = {
			sessionManager,
			settings: Settings.isolated({}),
			redactArtifactText: (text: string) => runner.redactArtifactText(text),
		} as unknown as AgentToolContext;

		const input: AgentToolResult = { content: [{ type: "text", text: rawOutput }] };
		const result = await postProcessToolResult(input, toolName, context);

		const artifactId = result.details?.meta?.truncation?.artifactId;
		const files = await fs.readdir(artifactsDir).catch(() => [] as string[]);
		const spillFile = files.find(file => file.endsWith(`.${toolName}.log`));
		const spillExists = spillFile !== undefined;
		const written = spillExists ? await Bun.file(path.join(artifactsDir, spillFile)).text() : "";
		return { result, artifactId, spillExists, spillFile, written };
	} finally {
		tempDir.removeSync();
	}
}

describe("MCP tool full-output spill artifact redaction", () => {
	it("writes the spilled full output with the extension-registered secret redacted", async () => {
		const { result, artifactId, spillFile, written } = await runSpill(
			`return { text: event.text.split(${JSON.stringify(secret)}).join("[REDACTED:TOKEN]") };`,
		);

		expect(artifactId).toBeDefined();
		expect(spillFile).toStartWith(`${artifactId}.`);
		expect(result.details?.meta?.artifactError).toBeUndefined();
		expect(written).not.toContain(secret);
		expect(written).toContain("[REDACTED:TOKEN]");
	});

	it("withholds the spill when the artifact_text handler throws", async () => {
		const { result, artifactId, spillExists, written } = await runSpill('throw new Error("redactor broke");');

		expect(spillExists).toBe(false);
		expect(written).not.toContain(secret);
		expect(artifactId).toBeUndefined();
		expect(result.details?.meta?.artifactError).toBe("redact");
		const text = result.content.map(block => (block.type === "text" ? block.text : "")).join("\n");
		expect(text).not.toContain(secret);
		expect(text).toContain("Full output was withheld");
	});

	it("withholds the spill when the artifact_text handler times out", async () => {
		testSetExtensionHandlerTimeoutMs(50);
		const { result, artifactId, spillExists, written } = await runSpill("return new Promise(() => {});");

		expect(spillExists).toBe(false);
		expect(written).not.toContain(secret);
		expect(artifactId).toBeUndefined();
		expect(result.details?.meta?.artifactError).toBe("redact");
		const text = result.content.map(block => (block.type === "text" ? block.text : "")).join("\n");
		expect(text).not.toContain(secret);
		expect(text).toContain("Full output was withheld");
	});
});
