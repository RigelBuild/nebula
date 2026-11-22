import { afterEach, describe, expect, it, mock, spyOn } from "bun:test";
import * as fs from "node:fs";
import * as path from "node:path";
import { AuthStorage, type ResetCreditAccountStatus, type UsageLimit, type UsageReport } from "@oh-my-pi/pi-ai";
import { getProjectAgentDir, logger, TempDir } from "@oh-my-pi/pi-utils";
import {
	claudeResetBankingFor,
	createGatewayClaudeResetHook,
	runGatewayClaudeResetSweep,
	type GatewayClaudeResetCoordinator,
} from "../../src/cli/auth-gateway-cli";
import { Settings } from "../../src/config/settings";
import { beginSettingsTest, restoreSettingsTestState } from "../helpers/settings-test-state";
import {
	cfgClaudeResetsApprovedRedeems,
	cfgClaudeResetsAutoRedeem5h,
	cfgClaudeResetsAutoRedeemFable,
	cfgClaudeResetsAutoRedeemWeekly,
	cfgClaudeResetsKeepCredits,
	cfgClaudeResetsMinBlockedMinutes,
	cfgOperatorClaudeResetsApprovedRedeems,
	type ClaudeResetApproval,
} from "../../src/session/settings";

const storages: AuthStorage[] = [];
afterEach(() => {
	for (const storage of storages.splice(0)) storage.close();
	mock.restore();
});

const NEEDS_APPROVAL = "auth-gateway Claude reset redeem needs approval";

function approval(windowResetsAt: number, expiresAt: string, accountId = "acct-cedar", creditsLeft = 1) {
	return {
		accountId,
		approvedBy: "matt",
		ref: "RIG-11726",
		expiresAt,
		windowResetsAt: new Date(windowResetsAt).toISOString(),
		creditsLeft,
	};
}

/** Writes entries as an operator could type them, unchecked; the gateway must tolerate any shape. */
function setRawApprovals(settings: Settings, entries: readonly unknown[]): void {
	cfgClaudeResetsApprovedRedeems.set(settings, entries as ClaudeResetApproval[]);
}

function limit(id: "anthropic:5h" | "anthropic:7d", usedFraction: number): UsageLimit {
	return {
		id,
		label: id,
		scope: { provider: "anthropic", windowId: id.endsWith("5h") ? "5h" : "7d", shared: true },
		window: {
			id,
			label: id,
			durationMs: id.endsWith("5h") ? 5 * 60 * 60_000 : 7 * 24 * 60 * 60_000,
			resetsAt: Date.now() + (id.endsWith("5h") ? 4 * 60 * 60_000 : 90 * 60_000),
		},
		amount: { unit: "percent", usedFraction, remainingFraction: 1 - usedFraction },
		status: usedFraction >= 1 ? "exhausted" : "ok",
	};
}

function status(report: UsageReport, clears: string[]): ResetCreditAccountStatus {
	return {
		provider: "anthropic",
		credentialId: 7,
		accountId: "acct-cedar",
		email: "cedar@example.com",
		active: true,
		availableCount: 1,
		redeemableCount: 1,
		eligible: true,
		nextCreditId: "cedar-credit",
		credits: [
			{ id: "cedar-credit", program: "cedar_ember", usable: true, remainingCount: 1, clears, blocking: clears },
		],
		report,
	};
}

/** The live banked credits of one account; the approval's `creditsLeft` is matched against their sum. */
function bankedCredits(
	usage: UsageReport,
	credentialId: number,
	credits: Array<{ id: string; remainingCount: number }>,
): ResetCreditAccountStatus {
	return {
		...status(usage, ["anthropic:7d"]),
		credentialId,
		availableCount: credits.reduce((sum, credit) => sum + credit.remainingCount, 0),
		nextCreditId: credits[0]?.id,
		credits: credits.map((credit, index) => ({
			id: credit.id,
			program: "cedar_ember",
			usable: index === 0,
			remainingCount: credit.remainingCount,
			clears: ["anthropic:7d"],
			blocking: ["anthropic:7d"],
		})),
	};
}

function coordinator(): GatewayClaudeResetCoordinator {
	return {
		attemptedKeys: new Set(),
		deferredUntilByKey: new Map(),
		lastAttemptAtByAccount: new Map(),
		inFlightByCredential: new Map(),
		spentApprovals: new Set(),
	};
}

async function setup(
	autoRedeem: "yes" | "no" | "unset",
	exhausted: "anthropic:5h" | "anthropic:7d",
	options: {
		drainFirst?: boolean;
		bankedReset?: boolean;
		keepCredits?: number;
		minBlockedMinutes?: number;
		accountless?: boolean;
		/** Writes an operator approval for the blocked window, so the spend is authorized. */
		approve?: boolean;
	} = {},
) {
	const bankedReset = options.bankedReset ?? true;
	const storage = await AuthStorage.create(":memory:");
	storages.push(storage);
	const credential = {
		type: "oauth" as const,
		access: "access-cedar",
		refresh: "refresh-cedar",
		expires: Date.now() + 60 * 60_000,
		...(options.accountless ? {} : { accountId: "acct-cedar" }),
		email: "cedar@example.com",
	};
	await storage.credentials.set("anthropic", [credential]);
	const stored = storage.credentials.list("anthropic")[0];
	if (!stored || stored.credential.type !== "oauth") throw new Error("expected stored Claude credential");
	const settings = Settings.isolated();
	cfgClaudeResetsAutoRedeem5h.set(settings, autoRedeem);
	cfgClaudeResetsAutoRedeemWeekly.set(settings, autoRedeem);
	cfgClaudeResetsAutoRedeemFable.set(settings, autoRedeem);
	cfgClaudeResetsKeepCredits.set(settings, options.keepCredits ?? 0);
	cfgClaudeResetsMinBlockedMinutes.set(settings, options.minBlockedMinutes ?? 60);
	if (options.drainFirst) {
		const account = options.accountless ? { email: "cedar@example.com" } : { accountId: "acct-cedar" };
		const accountPolicy = { provider: "anthropic", account, drainFirst: true };
		storage.setAccountPolicies({ accountPolicies: [accountPolicy], defaultReservePct: 10 });
	}
	const usage: UsageReport = {
		provider: "anthropic",
		fetchedAt: Date.now(),
		limits: [
			limit("anthropic:5h", exhausted === "anthropic:5h" ? 1 : 0.2),
			limit("anthropic:7d", exhausted === "anthropic:7d" ? 1 : 0.2),
		],
		metadata: { accountId: "acct-cedar", email: "cedar@example.com" },
		...(bankedReset
			? {
					resetCredits: {
						availableCount: 1,
						redeemableCount: 1,
						eligible: true,
						nextCreditId: "cedar-credit",
						credits: [
							{
								id: "cedar-credit",
								program: "cedar_ember",
								usable: true,
								remainingCount: 1,
								clears: ["anthropic:7d"],
							},
						],
					},
				}
			: {}),
	};
	const report = spyOn(storage.usage, "report").mockResolvedValue(usage);
	const statuses = bankedReset ? [{ ...status(usage, ["anthropic:7d"]), credentialId: stored.id }] : [];
	const list = spyOn(storage.resets, "list").mockResolvedValue(statuses);
	const redeem = spyOn(storage.resets, "redeem").mockResolvedValue({ ok: true, code: "reset", provider: "anthropic" });
	const hook = createGatewayClaudeResetHook({ storage, settings, coordinator: coordinator() });
	const windowResetsAt = usage.limits.find(limit => limit.id === "anthropic:7d")?.window?.resetsAt ?? 0;
	if (options.approve)
		cfgClaudeResetsApprovedRedeems.set(settings, [approval(windowResetsAt, "2099-01-01T00:00:00.000Z")]);
	return { hook, list, redeem, report, credentialId: stored.id, settings, storage, usage, windowResetsAt };
}

describe("gateway Claude reset hook", () => {
	describe("Claude reset banking configuration", () => {
		it("maps auto-redeem yes to general reset banking", () => {
			const settings = {
				autoRedeem5h: "yes",
				autoRedeemWeekly: "yes",
				autoRedeemFable: "yes",
				keepCredits: 2,
			} as const;
			expect(claudeResetBankingFor(settings)).toEqual({ keepCredits: 2 });
		});

		it("limits auto-redeem no reset banking to drain-first policies", () => {
			const settings = {
				autoRedeem5h: "no",
				autoRedeemWeekly: "no",
				autoRedeemFable: "no",
				keepCredits: 1,
			} as const;
			expect(claudeResetBankingFor(settings)).toEqual({
				keepCredits: 1,
				drainFirstOnly: true,
			});
		});

		it("does not enable reset banking when auto-redeem is unset", () => {
			const settings = {
				autoRedeem5h: "unset",
				autoRedeemWeekly: "unset",
				autoRedeemFable: "unset",
				keepCredits: 0,
			} as const;
			expect(claudeResetBankingFor(settings)).toBeUndefined();
		});
	});

	it("redeems an eligible Cedar reset covering the exhausted 7d limit once", async () => {
		const { hook, list, redeem, credentialId, report } = await setup("yes", "anthropic:7d", { approve: true });
		const signal = new AbortController().signal;
		expect(await hook({ provider: "anthropic", credentialId, modelId: "claude-sonnet", signal })).toBe(true);
		expect(report).toHaveBeenCalledTimes(1);
		expect(list).toHaveBeenCalledTimes(1);
		expect(redeem).toHaveBeenCalledTimes(1);
		expect(redeem.mock.calls[0]?.[0].target).toMatchObject({
			provider: "anthropic",
			credentialId,
			creditId: "cedar-credit",
		});
	});

	it("shares one redeem between concurrent 429s on the same credential", async () => {
		const { hook, redeem, credentialId } = await setup("yes", "anthropic:7d", { approve: true });
		const signal = new AbortController().signal;
		const results = await Promise.all([
			hook({ provider: "anthropic", credentialId, modelId: "claude-sonnet", signal }),
			hook({ provider: "anthropic", credentialId, modelId: "claude-sonnet", signal }),
		]);
		expect(results).toEqual([true, true]);
		expect(redeem).toHaveBeenCalledTimes(1);
	});

	it("does not list or redeem when auto-redeem is unset", async () => {
		const { hook, list, redeem, credentialId } = await setup("unset", "anthropic:7d");
		expect(
			await hook({
				provider: "anthropic",
				credentialId,
				modelId: "claude-sonnet",
				signal: new AbortController().signal,
			}),
		).toBe(false);
		expect(list).not.toHaveBeenCalled();
		expect(redeem).not.toHaveBeenCalled();
	});

	it("does not redeem a non-drain-first weekly reset without an approval, even with consented auto-redeem", async () => {
		const { hook, redeem, credentialId } = await setup("yes", "anthropic:7d");
		const warn = spyOn(logger, "warn").mockImplementation(() => {});
		expect(
			await hook({
				provider: "anthropic",
				credentialId,
				modelId: "claude-sonnet",
				signal: new AbortController().signal,
			}),
		).toBe(false);
		expect(redeem).not.toHaveBeenCalled();
		expect(warn).toHaveBeenCalledWith(
			NEEDS_APPROVAL,
			expect.objectContaining({ credentialId, accountId: "acct-cedar" }),
		);
	});

	it("does not redeem a drain-first weekly reset without an approval, and logs the need once", async () => {
		const { hook, list, redeem, credentialId, windowResetsAt } = await setup("no", "anthropic:7d", {
			drainFirst: true,
		});
		const warn = spyOn(logger, "warn").mockImplementation(() => {});
		const signal = new AbortController().signal;
		expect(await hook({ provider: "anthropic", credentialId, modelId: "claude-sonnet", signal })).toBe(false);
		expect(await hook({ provider: "anthropic", credentialId, modelId: "claude-sonnet", signal })).toBe(false);
		expect(list).toHaveBeenCalledTimes(2);
		expect(redeem).not.toHaveBeenCalled();
		const needs = warn.mock.calls.filter(([message]) => message === NEEDS_APPROVAL);
		expect(needs).toHaveLength(1);
		expect(needs[0]?.[1]).toMatchObject({
			credentialId,
			accountId: "acct-cedar",
			windowResetsAt: new Date(windowResetsAt).toISOString(),
		});
	});

	it("redeems a drain-first weekly reset once with a matching unexpired approval", async () => {
		const { hook, redeem, credentialId, settings, windowResetsAt } = await setup("no", "anthropic:7d", {
			drainFirst: true,
		});
		cfgClaudeResetsApprovedRedeems.set(settings, [approval(windowResetsAt, "2099-01-01T00:00:00.000Z")]);
		const warn = spyOn(logger, "warn").mockImplementation(() => {});
		expect(
			await hook({
				provider: "anthropic",
				credentialId,
				modelId: "claude-sonnet",
				signal: new AbortController().signal,
			}),
		).toBe(true);
		expect(redeem).toHaveBeenCalledTimes(1);
		expect(warn).toHaveBeenCalledWith(
			"auth-gateway Claude reset redeemed",
			expect.objectContaining({ credentialId, accountId: "acct-cedar", window: "weekly", approvalRef: "RIG-11726" }),
		);
	});
	it("redeems with an approval typed to the whole minute of a jittered live reset time", async () => {
		const { hook, redeem, credentialId, settings, windowResetsAt } = await setup("no", "anthropic:7d", {
			drainFirst: true,
		});
		// The live reset carries sub-minute jitter; the operator types only the minute.
		const wholeMinuteIso = new Date(Math.floor(windowResetsAt / 60_000) * 60_000).toISOString();
		cfgClaudeResetsApprovedRedeems.set(settings, [
			{
				accountId: "acct-cedar",
				approvedBy: "matt",
				ref: "RIG-11726",
				expiresAt: "2099-01-01T00:00:00.000Z",
				windowResetsAt: wholeMinuteIso,
				creditsLeft: 1,
			},
		]);
		expect(
			await hook({
				provider: "anthropic",
				credentialId,
				modelId: "claude-sonnet",
				signal: new AbortController().signal,
			}),
		).toBe(true);
		expect(redeem).toHaveBeenCalledTimes(1);
	});

	it("does not redeem with an expired approval", async () => {
		const { hook, redeem, credentialId, settings, windowResetsAt } = await setup("no", "anthropic:7d", {
			drainFirst: true,
		});
		cfgClaudeResetsApprovedRedeems.set(settings, [approval(windowResetsAt, "2000-01-01T00:00:00.000Z")]);
		expect(
			await hook({
				provider: "anthropic",
				credentialId,
				modelId: "claude-sonnet",
				signal: new AbortController().signal,
			}),
		).toBe(false);
		expect(redeem).not.toHaveBeenCalled();
	});

	it("does not redeem with an approval for another account", async () => {
		const { hook, redeem, credentialId, settings, windowResetsAt } = await setup("no", "anthropic:7d", {
			drainFirst: true,
		});
		cfgClaudeResetsApprovedRedeems.set(settings, [
			approval(windowResetsAt, "2099-01-01T00:00:00.000Z", "acct-other"),
		]);
		expect(
			await hook({
				provider: "anthropic",
				credentialId,
				modelId: "claude-sonnet",
				signal: new AbortController().signal,
			}),
		).toBe(false);
		expect(redeem).not.toHaveBeenCalled();
	});

	it("redeems an approved window once; a restarted gateway does not redeem the moved window", async () => {
		const { hook, list, redeem, report, credentialId, settings, storage, usage, windowResetsAt } = await setup(
			"no",
			"anthropic:7d",
			{ drainFirst: true },
		);
		cfgClaudeResetsApprovedRedeems.set(settings, [approval(windowResetsAt, "2099-01-01T00:00:00.000Z")]);
		const signal = new AbortController().signal;
		expect(await hook({ provider: "anthropic", credentialId, modelId: "claude-sonnet", signal })).toBe(true);
		expect(redeem).toHaveBeenCalledTimes(1);

		// The window rolls over: a new natural reset time, still exhausted, with the grant banked again.
		const nextWindowResetsAt = Date.now() + 6 * 24 * 60 * 60_000;
		const movedUsage: UsageReport = {
			...usage,
			limits: usage.limits.map(limit =>
				limit.id === "anthropic:7d"
					? { ...limit, window: { id: "anthropic:7d", label: "anthropic:7d", resetsAt: nextWindowResetsAt } }
					: limit,
			),
		};
		report.mockResolvedValue(movedUsage);
		list.mockResolvedValue([{ ...status(movedUsage, ["anthropic:7d"]), credentialId }]);
		const restarted = createGatewayClaudeResetHook({ storage, settings, coordinator: coordinator() });
		expect(await restarted({ provider: "anthropic", credentialId, modelId: "claude-sonnet", signal })).toBe(false);
		expect(redeem).toHaveBeenCalledTimes(1);

		// A fresh approval bound to the rolled-over window redeems that window once, and no more.
		cfgClaudeResetsApprovedRedeems.set(settings, [approval(nextWindowResetsAt, "2099-01-01T00:00:00.000Z")]);
		expect(await restarted({ provider: "anthropic", credentialId, modelId: "claude-sonnet", signal })).toBe(true);
		expect(redeem).toHaveBeenCalledTimes(2);
		expect(await restarted({ provider: "anthropic", credentialId, modelId: "claude-sonnet", signal })).toBe(false);
		expect(redeem).toHaveBeenCalledTimes(2);
	});
	describe("approval spend, entry validation and the operator layer", () => {
		const SKIPPED = "auth-gateway Claude reset approval skipped";
		const FUTURE = "2099-01-01T00:00:00.000Z";

		it("spends an approval once: a second credit banked on the same window is refused", async () => {
			const { hook, list, redeem, credentialId, settings, usage, windowResetsAt } = await setup(
				"no",
				"anthropic:7d",
				{
					drainFirst: true,
				},
			);
			const warn = spyOn(logger, "warn").mockImplementation(() => {});
			let nowMs = Date.now();
			spyOn(Date, "now").mockImplementation(() => nowMs);
			cfgClaudeResetsApprovedRedeems.set(settings, [approval(windowResetsAt, FUTURE, "acct-cedar", 2)]);
			const call = () =>
				hook({
					provider: "anthropic",
					credentialId,
					modelId: "claude-sonnet",
					signal: new AbortController().signal,
				});

			list.mockResolvedValue([bankedCredits(usage, credentialId, [{ id: "cedar-credit", remainingCount: 2 }])]);
			expect(await call()).toBe(true);
			expect(redeem).toHaveBeenCalledTimes(1);

			// The redeem spends one credit: the count falls to 1 and the window's resets_at does not move.
			list.mockResolvedValue([bankedCredits(usage, credentialId, [{ id: "cedar-credit", remainingCount: 1 }])]);
			nowMs += 2 * 60_000;
			expect(await call()).toBe(false);

			// A second credit is banked on the same window: the count is back to 2, so only the spent record refuses it.
			list.mockResolvedValue([
				bankedCredits(usage, credentialId, [
					{ id: "cedar-credit", remainingCount: 1 },
					{ id: "cedar-credit-2", remainingCount: 1 },
				]),
			]);
			nowMs += 2 * 60_000;
			expect(await call()).toBe(false);
			expect(redeem).toHaveBeenCalledTimes(1);
			expect(warn.mock.calls.filter(([message]) => message === NEEDS_APPROVAL)).toHaveLength(1);
		});

		it("does not redeem after a restart once the count has moved on", async () => {
			const { hook, list, redeem, credentialId, settings, storage, usage, windowResetsAt } = await setup(
				"no",
				"anthropic:7d",
				{ drainFirst: true },
			);
			cfgClaudeResetsApprovedRedeems.set(settings, [approval(windowResetsAt, FUTURE, "acct-cedar", 2)]);
			list.mockResolvedValue([bankedCredits(usage, credentialId, [{ id: "cedar-credit", remainingCount: 2 }])]);
			expect(
				await hook({
					provider: "anthropic",
					credentialId,
					modelId: "claude-sonnet",
					signal: new AbortController().signal,
				}),
			).toBe(true);

			// Restart: a new hook has no in-memory record. The redeem left one credit, so the count no longer matches.
			list.mockResolvedValue([bankedCredits(usage, credentialId, [{ id: "cedar-credit", remainingCount: 1 }])]);
			const restarted = createGatewayClaudeResetHook({ storage, settings, coordinator: coordinator() });
			expect(
				await restarted({
					provider: "anthropic",
					credentialId,
					modelId: "claude-sonnet",
					signal: new AbortController().signal,
				}),
			).toBe(false);
			expect(redeem).toHaveBeenCalledTimes(1);
		});

		it("does not match an approval whose creditsLeft differs from the live count", async () => {
			const { hook, list, redeem, credentialId, settings, usage, windowResetsAt } = await setup(
				"no",
				"anthropic:7d",
				{
					drainFirst: true,
				},
			);
			cfgClaudeResetsApprovedRedeems.set(settings, [approval(windowResetsAt, FUTURE, "acct-cedar", 2)]);
			list.mockResolvedValue([bankedCredits(usage, credentialId, [{ id: "cedar-credit", remainingCount: 1 }])]);
			expect(
				await hook({
					provider: "anthropic",
					credentialId,
					modelId: "claude-sonnet",
					signal: new AbortController().signal,
				}),
			).toBe(false);
			expect(redeem).not.toHaveBeenCalled();
		});

		it("skips malformed approval entries with one warn each, without throwing", async () => {
			const { hook, redeem, credentialId, settings, windowResetsAt } = await setup("no", "anthropic:7d", {
				drainFirst: true,
			});
			const warn = spyOn(logger, "warn").mockImplementation(() => {});
			const resetsAt = new Date(windowResetsAt).toISOString();
			setRawApprovals(settings, [
				null,
				"cedar",
				{ approvedBy: "matt", ref: "RIG-11726", expiresAt: FUTURE, windowResetsAt: resetsAt, creditsLeft: 1 },
				{
					accountId: "acct-cedar",
					approvedBy: "matt",
					ref: "RIG-11726",
					expiresAt: "not-a-date",
					windowResetsAt: resetsAt,
					creditsLeft: 1,
				},
				approval(windowResetsAt, FUTURE, "acct-cedar", 1),
			]);
			const call = () =>
				hook({
					provider: "anthropic",
					credentialId,
					modelId: "claude-sonnet",
					signal: new AbortController().signal,
				});
			expect(await call()).toBe(true);
			expect(redeem).toHaveBeenCalledTimes(1);
			expect(warn.mock.calls.filter(([message]) => message === SKIPPED)).toHaveLength(4);
			await call();
			expect(warn.mock.calls.filter(([message]) => message === SKIPPED)).toHaveLength(4);
		});

		it("does not match an approval without an accountId, even for a credential without one", async () => {
			const { hook, list, redeem, credentialId, settings, windowResetsAt } = await setup("no", "anthropic:7d", {
				drainFirst: true,
				accountless: true,
			});
			setRawApprovals(settings, [
				{
					approvedBy: "matt",
					ref: "RIG-11726",
					expiresAt: FUTURE,
					windowResetsAt: new Date(windowResetsAt).toISOString(),
					creditsLeft: 1,
				},
			]);
			expect(
				await hook({
					provider: "anthropic",
					credentialId,
					modelId: "claude-sonnet",
					signal: new AbortController().signal,
				}),
			).toBe(false);
			expect(list).toHaveBeenCalledTimes(1);
			expect(redeem).not.toHaveBeenCalled();
		});

		it("reads approvals from operator layers only: a project settings file cannot approve a redeem", async () => {
			const state = beginSettingsTest();
			const dir = TempDir.createSync("@pi-auth-gateway-approval-");
			try {
				const { list, redeem, credentialId, storage, windowResetsAt } = await setup("no", "anthropic:7d", {
					drainFirst: true,
				});
				const agentDir = dir.join("agent");
				const project = dir.join("project");
				fs.mkdirSync(agentDir, { recursive: true });
				fs.mkdirSync(getProjectAgentDir(project), { recursive: true });
				fs.writeFileSync(
					path.join(getProjectAgentDir(project), "settings.json"),
					JSON.stringify({
						claudeResets: {
							autoRedeem5h: "no",
							autoRedeemWeekly: "no",
							autoRedeemFable: "no",
							approvedRedeems: [approval(windowResetsAt, FUTURE)],
						},
					}),
				);
				const projectSettings = await Settings.loadIsolated({ cwd: project, agentDir });
				expect(cfgClaudeResetsApprovedRedeems.get(projectSettings)).toHaveLength(1);
				expect(cfgOperatorClaudeResetsApprovedRedeems.get(projectSettings)).toEqual([]);
				const projectHook = createGatewayClaudeResetHook({
					storage,
					settings: projectSettings,
					coordinator: coordinator(),
				});
				expect(
					await projectHook({
						provider: "anthropic",
						credentialId,
						modelId: "claude-sonnet",
						signal: new AbortController().signal,
					}),
				).toBe(false);
				expect(list).toHaveBeenCalledTimes(1);
				expect(redeem).not.toHaveBeenCalled();
			} finally {
				restoreSettingsTestState(state);
				dir.removeSync();
			}
		});

		it("matches an approval within 5 minutes of the window's reset, and not beyond", async () => {
			for (const [minutes, redeems] of [
				[-4, true],
				[4, true],
				[-6, false],
				[6, false],
			] as const) {
				const { hook, redeem, credentialId, settings, windowResetsAt } = await setup("no", "anthropic:7d", {
					drainFirst: true,
				});
				cfgClaudeResetsApprovedRedeems.set(settings, [approval(windowResetsAt + minutes * 60_000, FUTURE)]);
				expect(
					await hook({
						provider: "anthropic",
						credentialId,
						modelId: "claude-sonnet",
						signal: new AbortController().signal,
					}),
				).toBe(redeems);
				expect(redeem).toHaveBeenCalledTimes(redeems ? 1 : 0);
			}
		});

		it("keeps the approval gate after drain-first forces the weekly mode, with autoRedeemWeekly no and no approval", async () => {
			const { hook, list, redeem, credentialId, settings } = await setup("no", "anthropic:7d", { drainFirst: true });
			cfgClaudeResetsAutoRedeem5h.set(settings, "yes");
			cfgClaudeResetsAutoRedeemWeekly.set(settings, "no");
			const warn = spyOn(logger, "warn").mockImplementation(() => {});
			expect(
				await hook({
					provider: "anthropic",
					credentialId,
					modelId: "claude-sonnet",
					signal: new AbortController().signal,
				}),
			).toBe(false);
			expect(list).toHaveBeenCalledTimes(1);
			expect(redeem).not.toHaveBeenCalled();
			expect(warn.mock.calls.filter(([message]) => message === NEEDS_APPROVAL)).toHaveLength(1);
		});
	});

	it("does not redeem when auto-redeem is no and the account is not drain-first", async () => {
		const { hook, list, redeem, credentialId } = await setup("no", "anthropic:7d");
		expect(
			await hook({
				provider: "anthropic",
				credentialId,
				modelId: "claude-sonnet",
				signal: new AbortController().signal,
			}),
		).toBe(false);
		expect(list).not.toHaveBeenCalled();
		expect(redeem).not.toHaveBeenCalled();
	});

	it("keeps auto-redeem yes behavior for a drain-first account", async () => {
		const { hook, redeem, credentialId } = await setup("yes", "anthropic:7d", { drainFirst: true, approve: true });
		expect(
			await hook({
				provider: "anthropic",
				credentialId,
				modelId: "claude-sonnet",
				signal: new AbortController().signal,
			}),
		).toBe(true);
		expect(redeem).toHaveBeenCalledTimes(1);
	});
	it("keeps auto-redeem unset disabled for a drain-first account", async () => {
		const { hook, list, redeem, credentialId } = await setup("unset", "anthropic:7d", { drainFirst: true });
		expect(
			await hook({
				provider: "anthropic",
				credentialId,
				modelId: "claude-sonnet",
				signal: new AbortController().signal,
			}),
		).toBe(false);
		expect(list).not.toHaveBeenCalled();
		expect(redeem).not.toHaveBeenCalled();
	});

	it("keeps the last credit when drain-first auto-redeem has a credit reserve", async () => {
		const { hook, list, redeem, credentialId } = await setup("no", "anthropic:7d", {
			drainFirst: true,
			keepCredits: 1,
		});
		expect(
			await hook({
				provider: "anthropic",
				credentialId,
				modelId: "claude-sonnet",
				signal: new AbortController().signal,
			}),
		).toBe(false);
		expect(list).toHaveBeenCalledTimes(1);
		expect(redeem).not.toHaveBeenCalled();
	});

	it("keeps the minimum blocked-time rule for drain-first auto-redeem", async () => {
		const { hook, list, redeem, credentialId } = await setup("no", "anthropic:7d", {
			drainFirst: true,
			minBlockedMinutes: 120,
		});
		expect(
			await hook({
				provider: "anthropic",
				credentialId,
				modelId: "claude-sonnet",
				signal: new AbortController().signal,
			}),
		).toBe(false);
		expect(list).toHaveBeenCalledTimes(1);
		expect(redeem).not.toHaveBeenCalled();
	});

	it("does not redeem a drain-first account without a banked reset", async () => {
		const { hook, list, redeem, credentialId } = await setup("no", "anthropic:7d", {
			drainFirst: true,
			bankedReset: false,
		});
		expect(
			await hook({
				provider: "anthropic",
				credentialId,
				modelId: "claude-sonnet",
				signal: new AbortController().signal,
			}),
		).toBe(false);
		expect(list).not.toHaveBeenCalled();
		expect(redeem).not.toHaveBeenCalled();
	});

	it("does not spend a weekly credit for an exhausted 5h-only block", async () => {
		const { hook, list, redeem, credentialId } = await setup("yes", "anthropic:5h");
		expect(
			await hook({
				provider: "anthropic",
				credentialId,
				modelId: "claude-sonnet",
				signal: new AbortController().signal,
			}),
		).toBe(false);
		expect(list).not.toHaveBeenCalled();
		expect(redeem).not.toHaveBeenCalled();
	});

	it("does not spend a shared weekly reset when weekly consent is unset, even with 5h consent yes", async () => {
		const { hook, list, redeem, credentialId, settings } = await setup("yes", "anthropic:7d");
		cfgClaudeResetsAutoRedeemWeekly.set(settings, "unset");
		expect(
			await hook({
				provider: "anthropic",
				credentialId,
				modelId: "claude-sonnet",
				signal: new AbortController().signal,
			}),
		).toBe(false);
		expect(list).toHaveBeenCalledTimes(1);
		expect(redeem).not.toHaveBeenCalled();
	});

	it("sweeps only 7d-exhausted accounts with reset inventory", async () => {
		const storage = await AuthStorage.create(":memory:");
		storages.push(storage);
		const accounts = [
			{ credentialId: 7, accountId: "acct-cedar", email: "cedar@example.com", active: false, position: 0 },
			{ credentialId: 8, accountId: "acct-half", email: "half@example.com", active: false, position: 1 },
			{ credentialId: 9, accountId: "acct-no-inventory", email: "none@example.com", active: false, position: 2 },
			{ credentialId: 10, accountId: "acct-second", email: "second@example.com", active: false, position: 3 },
		];
		spyOn(storage.oauth, "accounts").mockReturnValue(accounts);
		const report = (accountId: string, weekly: number, inventory: boolean): UsageReport => ({
			provider: "anthropic",
			fetchedAt: Date.now(),
			limits: [limit("anthropic:7d", weekly)],
			metadata: { accountId },
			...(inventory
				? {
						resetCredits: {
							availableCount: 1,
							redeemableCount: 1,
							eligible: true,
							nextCreditId: "cedar-credit",
							credits: [{ id: "cedar-credit", usable: true, clears: ["anthropic:7d"] }],
						},
					}
				: {}),
		});
		const reports = spyOn(storage.usage, "reports").mockResolvedValue([
			report("acct-cedar", 1, true),
			report("acct-half", 0.5, true),
			report("acct-no-inventory", 1, false),
			report("acct-second", 1, true),
		]);
		const calls: Array<{ provider: string; credentialId: number; modelId: string; signal: AbortSignal }> = [];
		const controller = new AbortController();
		await runGatewayClaudeResetSweep({
			storage,
			signal: controller.signal,
			hook: async ctx => {
				calls.push(ctx);
				return false;
			},
		});
		const reportOptions = reports.mock.calls[0]?.[0];
		expect(reportOptions?.signal).not.toBe(controller.signal);
		expect(reportOptions?.signal?.aborted).toBe(false);
		const sweepSignal = calls[0]?.signal;
		expect(sweepSignal).toBeInstanceOf(AbortSignal);
		expect(sweepSignal?.aborted).toBe(false);
		expect(calls.map(call => call.credentialId)).toEqual([7, 10]);
		expect(calls[1]?.signal).not.toBe(calls[0]?.signal);
		expect(calls[0]).toMatchObject({ provider: "anthropic", modelId: "" });
	});

	it("logs the need-approval warn line once per account when two accounts share a reset time", async () => {
		const storage = await AuthStorage.create(":memory:");
		storages.push(storage);
		await storage.credentials.set("anthropic", [
			{
				type: "oauth" as const,
				access: "cedar",
				refresh: "cedar",
				expires: Date.now() + 60_000,
				accountId: "acct-cedar",
			},
			{
				type: "oauth" as const,
				access: "sibling",
				refresh: "sibling",
				expires: Date.now() + 60_000,
				accountId: "acct-sibling",
			},
		]);
		const storedCredentials = storage.credentials.list("anthropic");
		const cedarId = storedCredentials.find(
			row => row.credential.type === "oauth" && row.credential.accountId === "acct-cedar",
		)?.id;
		const siblingId = storedCredentials.find(
			row => row.credential.type === "oauth" && row.credential.accountId === "acct-sibling",
		)?.id;
		if (cedarId === undefined || siblingId === undefined) throw new Error("expected both stored Claude accounts");
		storage.setAccountPolicies({
			accountPolicies: [
				{ provider: "anthropic", account: { accountId: "acct-cedar" }, drainFirst: true },
				{ provider: "anthropic", account: { accountId: "acct-sibling" }, drainFirst: true },
			],
			defaultReservePct: 10,
		});
		// Both accounts exhaust the same weekly window, so they share one natural reset time.
		const windowResetsAt = Date.now() + 90 * 60_000;
		const resetReport = (accountId: string, creditId: string): UsageReport => ({
			provider: "anthropic",
			fetchedAt: Date.now(),
			limits: [{ ...limit("anthropic:7d", 1), window: { id: "7d", label: "7d", resetsAt: windowResetsAt } }],
			metadata: { accountId },
			resetCredits: {
				availableCount: 1,
				redeemableCount: 1,
				eligible: true,
				nextCreditId: creditId,
				credits: [{ id: creditId, usable: true, clears: ["anthropic:7d"] }],
			},
		});
		spyOn(storage.usage, "reports").mockResolvedValue([
			resetReport("acct-cedar", "cedar-credit"),
			resetReport("acct-sibling", "sibling-credit"),
		]);
		spyOn(storage.usage, "report").mockImplementation(async (_provider, credential) =>
			credential.type === "oauth" && credential.accountId === "acct-sibling"
				? resetReport("acct-sibling", "sibling-credit")
				: resetReport("acct-cedar", "cedar-credit"),
		);
		spyOn(storage.resets, "list").mockResolvedValue([
			{ ...status(resetReport("acct-cedar", "cedar-credit"), ["anthropic:7d"]), credentialId: cedarId },
			{ ...status(resetReport("acct-sibling", "sibling-credit"), ["anthropic:7d"]), credentialId: siblingId },
		]);
		const redeem = spyOn(storage.resets, "redeem").mockResolvedValue({
			ok: true,
			code: "reset",
			provider: "anthropic",
		});
		const warn = spyOn(logger, "warn").mockImplementation(() => {});
		const settings = Settings.isolated();
		cfgClaudeResetsAutoRedeem5h.set(settings, "no");
		cfgClaudeResetsAutoRedeemWeekly.set(settings, "no");
		cfgClaudeResetsAutoRedeemFable.set(settings, "no");
		const hook = createGatewayClaudeResetHook({ storage, settings, coordinator: coordinator() });
		await runGatewayClaudeResetSweep({
			storage,
			signal: new AbortController().signal,
			hook: async context => hook(context),
		});
		expect(redeem).not.toHaveBeenCalled();
		const needs = warn.mock.calls.filter(([message]) => message === NEEDS_APPROVAL);
		expect(needs).toHaveLength(2);
		expect(needs[0]?.[1]).toEqual(expect.objectContaining({ accountId: "acct-cedar" }));
		expect(needs[1]?.[1]).toEqual(expect.objectContaining({ accountId: "acct-sibling" }));
	});

	it("sweeps only drain-first accounts when auto-redeem is no", async () => {
		const storage = await AuthStorage.create(":memory:");
		storages.push(storage);
		const credentials = [
			{
				type: "oauth" as const,
				access: "cedar",
				refresh: "cedar",
				expires: Date.now() + 60_000,
				accountId: "acct-cedar",
			},
			{
				type: "oauth" as const,
				access: "sibling",
				refresh: "sibling",
				expires: Date.now() + 60_000,
				accountId: "acct-sibling",
			},
		];
		await storage.credentials.set("anthropic", credentials);
		const storedCredentials = storage.credentials.list("anthropic");
		const drainId = storedCredentials.find(
			row => row.credential.type === "oauth" && row.credential.accountId === "acct-cedar",
		)?.id;
		const siblingId = storedCredentials.find(
			row => row.credential.type === "oauth" && row.credential.accountId === "acct-sibling",
		)?.id;
		if (drainId === undefined || siblingId === undefined) throw new Error("expected both stored Claude accounts");
		storage.setAccountPolicies({
			accountPolicies: [{ provider: "anthropic", account: { accountId: "acct-cedar" }, drainFirst: true }],
			defaultReservePct: 10,
		});
		const windowResetsAt = Date.now() + 90 * 60_000;
		const resetReport = (accountId: string, creditId: string): UsageReport => ({
			provider: "anthropic",
			fetchedAt: Date.now(),
			limits: [{ ...limit("anthropic:7d", 1), window: { id: "7d", label: "7d", resetsAt: windowResetsAt } }],
			metadata: { accountId },
			resetCredits: {
				availableCount: 1,
				redeemableCount: 1,
				eligible: true,
				nextCreditId: creditId,
				credits: [{ id: creditId, usable: true, clears: ["anthropic:7d"] }],
			},
		});
		spyOn(storage.usage, "reports").mockResolvedValue([
			resetReport("acct-cedar", "cedar-credit"),
			resetReport("acct-sibling", "sibling-credit"),
		]);
		spyOn(storage.usage, "report").mockImplementation(async (_provider, credential) =>
			credential.type === "oauth" && credential.accountId === "acct-sibling"
				? resetReport("acct-sibling", "sibling-credit")
				: resetReport("acct-cedar", "cedar-credit"),
		);
		const statuses = [
			{ ...status(resetReport("acct-cedar", "cedar-credit"), ["anthropic:7d"]), credentialId: drainId },
			{ ...status(resetReport("acct-sibling", "sibling-credit"), ["anthropic:7d"]), credentialId: siblingId },
		];
		const list = spyOn(storage.resets, "list").mockResolvedValue(statuses);
		const redeem = spyOn(storage.resets, "redeem").mockResolvedValue({
			ok: true,
			code: "reset",
			provider: "anthropic",
		});
		const settings = Settings.isolated();
		cfgClaudeResetsApprovedRedeems.set(settings, [
			approval(windowResetsAt, "2099-01-01T00:00:00.000Z", "acct-cedar"),
		]);
		cfgClaudeResetsAutoRedeem5h.set(settings, "no");
		cfgClaudeResetsAutoRedeemWeekly.set(settings, "no");
		cfgClaudeResetsAutoRedeemFable.set(settings, "no");
		const hook = createGatewayClaudeResetHook({ storage, settings, coordinator: coordinator() });
		const hooked: number[] = [];
		await runGatewayClaudeResetSweep({
			storage,
			signal: new AbortController().signal,
			hook: async context => {
				hooked.push(context.credentialId);
				return hook(context);
			},
		});
		expect(hooked).toEqual([drainId, siblingId]);
		expect(list).toHaveBeenCalledTimes(1);
		expect(redeem).toHaveBeenCalledTimes(1);
		expect(redeem.mock.calls[0]?.[0].target.credentialId).toBe(drainId);
	});
});

describe("gateway Claude reset hook for Fable pins", () => {
	function fableUsage(exhaustedFable: boolean): UsageReport {
		return {
			provider: "anthropic",
			fetchedAt: Date.now(),
			limits: [
				limit("anthropic:5h", 0.2),
				limit("anthropic:7d", 0.2),
				{
					id: "anthropic:7d:fable",
					label: "Claude 7 Day (Fable)",
					scope: { provider: "anthropic", windowId: "7d", tier: "fable" },
					window: { id: "7d", label: "7 Day", resetsAt: Date.now() + 90 * 60_000 },
					amount: {
						unit: "percent",
						used: exhaustedFable ? 100 : 40,
						limit: 100,
						usedFraction: exhaustedFable ? 1 : 0.4,
						remainingFraction: exhaustedFable ? 0 : 0.6,
					},
					status: exhaustedFable ? "exhausted" : "ok",
				},
			],
			metadata: { accountId: "acct-cedar", email: "cedar@example.com" },
			resetCredits: {
				availableCount: 1,
				redeemableCount: 1,
				eligible: true,
				nextCreditId: "fable-grant",
				credits: [
					{
						id: "fable-grant",
						program: "cedar_ember",
						usable: true,
						remainingCount: 1,
						clears: ["anthropic:7d:fable"],
						blocking: ["anthropic:7d:fable"],
					},
				],
			},
		};
	}

	async function setupFable(
		grantClears: string[],
		fableConsent: "yes" | "unset" = "yes",
		weeklyConsent: "yes" | "no" = "no",
		approve = false,
	) {
		const storage = await AuthStorage.create(":memory:");
		storages.push(storage);
		const credential = {
			type: "oauth" as const,
			access: "access-cedar",
			refresh: "refresh-cedar",
			expires: Date.now() + 60 * 60_000,
			accountId: "acct-cedar",
			email: "cedar@example.com",
		};
		await storage.credentials.set("anthropic", [credential]);
		const stored = storage.credentials.list("anthropic")[0];
		if (!stored || stored.credential.type !== "oauth") throw new Error("expected stored Claude credential");
		const settings = Settings.isolated();
		cfgClaudeResetsAutoRedeemFable.set(settings, fableConsent);
		cfgClaudeResetsAutoRedeemWeekly.set(settings, weeklyConsent);
		cfgClaudeResetsKeepCredits.set(settings, 0);
		cfgClaudeResetsMinBlockedMinutes.set(settings, 60);
		const usage = fableUsage(true);
		const statuses = [
			{
				...status(usage, grantClears),
				credentialId: stored.id,
				credits: [
					{
						id: "fable-grant",
						program: "cedar_ember",
						usable: true,
						remainingCount: 1,
						clears: grantClears,
						blocking: grantClears,
					},
				],
				nextCreditId: "fable-grant",
			},
		];
		spyOn(storage.usage, "report").mockResolvedValue(usage);
		spyOn(storage.resets, "list").mockResolvedValue(statuses);
		const redeem = spyOn(storage.resets, "redeem").mockResolvedValue({
			ok: true,
			code: "reset",
			provider: "anthropic",
		});
		const hook = createGatewayClaudeResetHook({ storage, settings, coordinator: coordinator() });
		const fableReset = usage.limits.find(limit => limit.id === "anthropic:7d:fable")?.window?.resetsAt ?? 0;
		if (approve) cfgClaudeResetsApprovedRedeems.set(settings, [approval(fableReset, "2099-01-01T00:00:00.000Z")]);
		return { hook, redeem, credentialId: stored.id };
	}

	it("redeems a Fable launch grant when a Fable-model request hits an exhausted Fable window", async () => {
		const { hook, redeem, credentialId } = await setupFable(["anthropic:7d:fable"], "yes", "no", true);
		const result = await hook({
			provider: "anthropic",
			credentialId,
			modelId: "claude-fable-5",
			signal: new AbortController().signal,
		});
		expect(result).toBe(true);
		expect(redeem).toHaveBeenCalledTimes(1);
		expect(redeem.mock.calls[0]?.[0].target).toMatchObject({ credentialId, creditId: "fable-grant" });
	});

	it("does not spend a full-week grant for an exhausted Fable window", async () => {
		const { hook, redeem, credentialId } = await setupFable(["anthropic:7d", "anthropic:7d:fable"]);
		const result = await hook({
			provider: "anthropic",
			credentialId,
			modelId: "claude-fable-5",
			signal: new AbortController().signal,
		});
		expect(result).toBe(false);
		expect(redeem).not.toHaveBeenCalled();
	});

	it("refuses a consented full-week grant when only the Fable window is blocked", async () => {
		const { hook, redeem, credentialId } = await setupFable(["anthropic:7d", "anthropic:7d:fable"], "yes", "yes");
		const result = await hook({
			provider: "anthropic",
			credentialId,
			modelId: "claude-fable-5",
			signal: new AbortController().signal,
		});
		expect(result).toBe(false);
		expect(redeem).not.toHaveBeenCalled();
	});

	it("treats a non-Fable selected grant as no Fable reset and does not redeem it", async () => {
		const { hook, redeem, credentialId } = await setupFable(["anthropic:7d"]);
		const result = await hook({
			provider: "anthropic",
			credentialId,
			modelId: "claude-fable-5",
			signal: new AbortController().signal,
		});
		expect(result).toBe(false);
		expect(redeem).not.toHaveBeenCalled();
	});

	it("redeems a full-week grant for a Fable request blocked by the exhausted shared weekly window", async () => {
		const usage: UsageReport = {
			provider: "anthropic",
			fetchedAt: Date.now(),
			limits: [
				limit("anthropic:5h", 0.2),
				limit("anthropic:7d", 1),
				{
					...fableUsage(false).limits[2]!,
					amount: { unit: "percent", used: 30, limit: 100, usedFraction: 0.3, remainingFraction: 0.7 },
				},
			],
			metadata: { accountId: "acct-cedar", email: "cedar@example.com" },
			resetCredits: {
				availableCount: 1,
				redeemableCount: 1,
				eligible: true,
				nextCreditId: "cedar-credit",
				credits: [
					{
						id: "cedar-credit",
						program: "cedar_ember",
						usable: true,
						remainingCount: 1,
						clears: ["anthropic:7d"],
						blocking: ["anthropic:7d"],
					},
				],
			},
		};
		const storage = await AuthStorage.create(":memory:");
		storages.push(storage);
		await storage.credentials.set("anthropic", [
			{
				type: "oauth" as const,
				access: "access-cedar",
				refresh: "refresh-cedar",
				expires: Date.now() + 60 * 60_000,
				accountId: "acct-cedar",
				email: "cedar@example.com",
			},
		]);
		const stored = storage.credentials.list("anthropic")[0];
		if (!stored) throw new Error("expected stored Claude credential");
		const settings = Settings.isolated();
		cfgClaudeResetsAutoRedeemFable.set(settings, "yes");
		cfgClaudeResetsAutoRedeemWeekly.set(settings, "yes");
		cfgClaudeResetsKeepCredits.set(settings, 0);
		cfgClaudeResetsMinBlockedMinutes.set(settings, 60);
		spyOn(storage.usage, "report").mockResolvedValue(usage);
		spyOn(storage.resets, "list").mockResolvedValue([
			{ ...status(usage, ["anthropic:7d"]), credentialId: stored.id },
		]);
		const redeem = spyOn(storage.resets, "redeem").mockResolvedValue({
			ok: true,
			code: "reset",
			provider: "anthropic",
		});
		const hook = createGatewayClaudeResetHook({ storage, settings, coordinator: coordinator() });
		const weeklyReset = usage.limits.find(limit => limit.id === "anthropic:7d")?.window?.resetsAt ?? 0;
		cfgClaudeResetsApprovedRedeems.set(settings, [approval(weeklyReset, "2099-01-01T00:00:00.000Z")]);

		const result = await hook({
			provider: "anthropic",
			credentialId: stored.id,
			modelId: "claude-fable-5",
			signal: new AbortController().signal,
		});

		expect(result).toBe(true);
		expect(redeem).toHaveBeenCalledTimes(1);
		expect(redeem.mock.calls[0]?.[0].target).toMatchObject({ credentialId: stored.id, creditId: "cedar-credit" });
	});

	it("does not spend a Fable grant when Fable auto-redeem is unset", async () => {
		const { hook, redeem, credentialId } = await setupFable(["anthropic:7d:fable"], "unset", "yes");
		const result = await hook({
			provider: "anthropic",
			credentialId,
			modelId: "claude-fable-5",
			signal: new AbortController().signal,
		});
		expect(result).toBe(false);
		expect(redeem).not.toHaveBeenCalled();
	});
});
