import { describe, expect, it } from "bun:test";
import type { UsageResetCredits } from "@oh-my-pi/pi-ai";
import { summarizeUsageResetCredits } from "@oh-my-pi/pi-tui/overlays/usage-display";

const now = Date.parse("2026-06-01T00:00:00.000Z");

describe("summarizeUsageResetCredits", () => {
	it("carries the per-scope split through for a multi-scope report", () => {
		const reset: UsageResetCredits = {
			availableCount: 3,
			redeemableCount: 1,
			availableByScope: { full: 1, fable: 2 },
			nextCreditId: "grant_full",
			eligible: true,
			credits: [{ id: "grant_full", remainingCount: 1, usable: true, status: "available" }],
		};
		expect(summarizeUsageResetCredits(reset, now)).toMatchObject({
			bankedCount: 3,
			redeemableCount: 1,
			availableByScope: { full: 1, fable: 2 },
		});
	});

	it("keeps a single-scope report unchanged", () => {
		const reset: UsageResetCredits = {
			availableCount: 2,
			redeemableCount: 2,
			availableByScope: { full: 2, fable: 0 },
			eligible: true,
		};
		const summary = summarizeUsageResetCredits(reset, now);
		expect(summary).toMatchObject({ bankedCount: 2, redeemableCount: 2, availableByScope: { full: 2, fable: 0 } });
	});

	it("omits the split when the provider reports no scopes", () => {
		const summary = summarizeUsageResetCredits({ availableCount: 2, redeemableCount: 2, eligible: true }, now);
		expect(summary).toBeDefined();
		expect(summary).not.toHaveProperty("availableByScope");
	});

	it("returns undefined without reset metadata", () => {
		expect(summarizeUsageResetCredits(undefined, now)).toBeUndefined();
	});
});
