import { afterEach, describe, expect, it, vi } from "bun:test";
import { Effort } from "@oh-my-pi/pi-catalog/effort";
import { type } from "@oh-my-pi/omptype";
import { Agent, type AgentTool, Tokenizer } from "@oh-my-pi/pi-agent-core";
import { createMockModel } from "@oh-my-pi/pi-ai/providers/mock";
import { resolveAutolearnCaptureTarget } from "@oh-my-pi/pi-coding-agent/autolearn/capture-target";
import { Settings } from "@oh-my-pi/pi-coding-agent/config/settings";
import { createAutoLearnCaptureRunner } from "@oh-my-pi/pi-coding-agent/sdk";
import { type CustomMessage, convertToLlm } from "@oh-my-pi/pi-coding-agent/session/messages";
import { logger } from "@oh-my-pi/pi-utils";

const manageSkill: AgentTool = {
	name: "manage_skill",
	label: "manage_skill",
	description: "Manage reusable skills",
	parameters: type({}),
	execute: async () => ({ content: [{ type: "text", text: "captured" }] }),
};

/**
 * A seat with history. `reasoningOn` false reasons off, so a capture that forces reasoning on is visible; true
 * reasons at High, so a role that turns reasoning off is visible against a seat that had it on.
 */
async function seatWithHistory(reasoningOn: boolean) {
	const seat = createMockModel({ id: "seat-model", provider: "anthropic", responses: [{ content: ["Answer."] }] });
	const sourceAgent = new Agent({
		initialState: {
			model: seat,
			systemPrompt: ["Primary system prompt"],
			tools: [manageSkill],
			thinkingLevel: Effort.High,
			disableReasoning: !reasoningOn,
		},
		convertToLlm,
		streamFn: seat.stream,
	});
	await sourceAgent.prompt("Earlier task");
	return { seat, sourceAgent };
}

function roleSettings(value: string): Settings {
	const settings = Settings.isolated({ "autolearn.captureRole": "judge" });
	settings.setModelRole("judge", value);
	return settings;
}

afterEach(() => vi.restoreAllMocks());

describe("autolearn capture role thinking selectors (M1)", () => {
	it("a role set to off forces reasoning off even on a high session", () => {
		const luna = createMockModel({ id: "luna", provider: "luna" });
		const target = resolveAutolearnCaptureTarget(roleSettings("luna/luna:off"), [luna], Effort.High);
		expect(target?.thinkingLevel).toBe("off");
	});

	it("a role set to auto takes the model's provisional level, not the seat's", () => {
		const luna = createMockModel({ id: "luna", provider: "luna", reasoning: true });
		// A model with a thinking ladder has a provisional level for auto; the seat's Low must not win.
		Object.assign(luna, { thinking: { efforts: [Effort.Low, Effort.Medium, Effort.High] } });
		const target = resolveAutolearnCaptureTarget(roleSettings("luna/luna:auto"), [luna], Effort.Low);
		expect(target?.thinkingLevel).toBe(Effort.High);
	});

	it("clamps a fallback level the role model cannot run, instead of passing the seat's max through", () => {
		const luna = createMockModel({ id: "luna", provider: "luna", reasoning: true });
		// The ladder stops at xhigh: a seat at max must not reach the role model as max.
		Object.assign(luna, { thinking: { efforts: [Effort.Low, Effort.Medium, Effort.High, Effort.XHigh] } });
		const target = resolveAutolearnCaptureTarget(roleSettings("luna/luna"), [luna], Effort.Max);
		expect(target?.thinkingLevel).not.toBe(Effort.Max);
		expect(target?.thinkingLevel).toBe(Effort.XHigh);
	});
});

describe("autolearn capture routing and forced reasoning (M2)", () => {
	it("runs the capture on the role's model, not the seat's", async () => {
		const { seat, sourceAgent } = await seatWithHistory(false);
		const role = createMockModel({ id: "luna", provider: "luna", responses: [{ content: ["Kept."] }] });
		const built: Array<{ id: string | undefined }> = [];
		const run = createAutoLearnCaptureRunner({
			sourceAgent,
			captureTools: () => [manageSkill],
			createAgent: options => {
				built.push({ id: options.initialState?.model?.id });
				return new Agent({ ...options, convertToLlm, streamFn: role.stream });
			},
			resolveCaptureTarget: () => ({ model: role, thinkingLevel: Effort.Medium }),
		});
		await run("Automated capture prompt");
		expect(built).toEqual([{ id: "luna" }]);
		expect(seat.calls).toHaveLength(1);
		expect(role.calls).toHaveLength(1);
	});

	it("forces reasoning on for a role that names a level, even when the seat has it off", async () => {
		const { sourceAgent } = await seatWithHistory(false);
		const role = createMockModel({ id: "luna", provider: "luna", responses: [{ content: ["Kept."] }] });
		const run = createAutoLearnCaptureRunner({
			sourceAgent,
			captureTools: () => [manageSkill],
			createAgent: options => new Agent({ ...options, convertToLlm, streamFn: role.stream }),
			resolveCaptureTarget: () => ({ model: role, thinkingLevel: Effort.High }),
		});
		await run("Automated capture prompt");
		expect(role.calls[0]?.options?.disableReasoning).toBe(false);
		expect(role.calls[0]?.options?.reasoning).toBe(Effort.High);
	});

	it("keeps reasoning off for a role set to off, on a seat that has it on", async () => {
		const { sourceAgent } = await seatWithHistory(true);
		const role = createMockModel({ id: "luna", provider: "luna", responses: [{ content: ["Kept."] }] });
		const run = createAutoLearnCaptureRunner({
			sourceAgent,
			captureTools: () => [manageSkill],
			createAgent: options => new Agent({ ...options, convertToLlm, streamFn: role.stream }),
			resolveCaptureTarget: () => ({ model: role, thinkingLevel: "off" }),
		});
		await run("Automated capture prompt");
		expect(role.calls[0]?.options?.disableReasoning).toBe(true);
	});
});

describe("autolearn capture digest guard (L2)", () => {
	it("skips the capture and warns when the system prompt alone exceeds captureMaxInputTokens", async () => {
		const { seat, sourceAgent } = await seatWithHistory(false);
		const warned = vi.spyOn(logger, "warn").mockImplementation(() => {});
		const run = createAutoLearnCaptureRunner({
			sourceAgent,
			captureTools: () => [manageSkill],
			createAgent: options => new Agent({ ...options, convertToLlm, streamFn: seat.stream }),
			maxInputTokens: 1,
		});
		await run("Automated capture prompt");
		expect(seat.calls).toHaveLength(1);
		expect(warned).toHaveBeenCalledWith(expect.stringContaining("captureMaxInputTokens"), expect.any(Object));
	});

	it("skips the capture when the fixed prompt is exactly at the cap, leaving no replay budget", async () => {
		const { seat, sourceAgent } = await seatWithHistory(false);
		const warned = vi.spyOn(logger, "warn").mockImplementation(() => {});
		// Measure the fixed part (system prompt and tools) with the runner's own tokenizer, then cap at exactly that.
		const tokenizer = new Tokenizer(seat);
		const fixed =
			tokenizer.countTokens(["Primary system prompt"]) +
			tokenizer.countTokens(
				[manageSkill].map(tool => `${tool.name}\n${tool.description}\n${JSON.stringify(tool.parameters)}`),
			) +
			tokenizer.countMessage({
				role: "custom",
				customType: "autolearn-nudge",
				content: "Automated capture prompt",
				display: false,
				attribution: "agent",
				timestamp: 0,
			} as CustomMessage);
		const run = createAutoLearnCaptureRunner({
			sourceAgent,
			captureTools: () => [manageSkill],
			createAgent: options => new Agent({ ...options, convertToLlm, streamFn: seat.stream }),
			maxInputTokens: fixed,
		});
		await run("Automated capture prompt");
		expect(seat.calls).toHaveLength(1);
		expect(warned).toHaveBeenCalledWith(expect.stringContaining("captureMaxInputTokens"), expect.any(Object));
	});
});
