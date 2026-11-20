import { afterEach, describe, expect, it, vi } from "bun:test";
import { Effort } from "@oh-my-pi/pi-catalog/effort";
import { type } from "@oh-my-pi/omptype";
import { Agent, type AgentTool } from "@oh-my-pi/pi-agent-core";
import { createMockModel } from "@oh-my-pi/pi-ai/providers/mock";
import { resolveAutolearnCaptureTarget } from "@oh-my-pi/pi-coding-agent/autolearn/capture-target";
import { Settings } from "@oh-my-pi/pi-coding-agent/config/settings";
import { createAutoLearnCaptureRunner } from "@oh-my-pi/pi-coding-agent/sdk";
import { convertToLlm } from "@oh-my-pi/pi-coding-agent/session/messages";
import { logger } from "@oh-my-pi/pi-utils";

const manageSkill: AgentTool = {
	name: "manage_skill",
	label: "manage_skill",
	description: "Manage reusable skills",
	parameters: type({}),
	execute: async () => ({ content: [{ type: "text", text: "captured" }] }),
};

/** A seat that already has history and reasons off, so a capture that forces reasoning on is visible. */
async function reasoningOffSeat() {
	const seat = createMockModel({ id: "seat-model", provider: "anthropic", responses: [{ content: ["Answer."] }] });
	const sourceAgent = new Agent({
		initialState: {
			model: seat,
			systemPrompt: ["Primary system prompt"],
			tools: [manageSkill],
			thinkingLevel: Effort.High,
			disableReasoning: true,
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
});

describe("autolearn capture routing and forced reasoning (M2)", () => {
	it("runs the capture on the role's model, not the seat's", async () => {
		const { seat, sourceAgent } = await reasoningOffSeat();
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
		const { sourceAgent } = await reasoningOffSeat();
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
		const { sourceAgent } = await reasoningOffSeat();
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
		const { seat, sourceAgent } = await reasoningOffSeat();
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
});
