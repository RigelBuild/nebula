import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "bun:test";
import { Agent } from "@oh-my-pi/pi-agent-core";
import type {
	AuthCredential,
	ResetCreditAccountStatus,
	ResetCreditTarget,
	SessionRestrictionLease,
	UsageReport,
} from "@oh-my-pi/pi-ai";
import { createMockModel } from "@oh-my-pi/pi-ai/providers/mock";
import * as envApiKey from "@oh-my-pi/pi-ai/env-api-key";
import { closeModelCache } from "@oh-my-pi/pi-catalog/model-cache";
import { getBundledModel } from "@oh-my-pi/pi-catalog/models";
import { runUsageCommand } from "@oh-my-pi/pi-coding-agent/cli/usage-cli";
import { ModelRegistry } from "@oh-my-pi/pi-coding-agent/config/model-registry";
import { Settings } from "@oh-my-pi/pi-coding-agent/config/settings";
import { ExtensionRuntime } from "@oh-my-pi/pi-coding-agent/extensibility/extensions/loader";
import { ExtensionRunner } from "@oh-my-pi/pi-coding-agent/extensibility/extensions/runner";
import { initializeExtensions } from "@oh-my-pi/pi-coding-agent/modes/runtime-init";
import * as sdkModule from "@oh-my-pi/pi-coding-agent/sdk";
import { AgentSession } from "@oh-my-pi/pi-coding-agent/session/agent-session";
import { AuthStorage } from "@oh-my-pi/pi-coding-agent/session/auth-storage";
import {
	type CodexAutoRedeemCoordinator,
	createCodexAutoRedeemCoordinator,
} from "@oh-my-pi/pi-coding-agent/session/codex-auto-reset";
import { SessionManager } from "@oh-my-pi/pi-coding-agent/session/session-manager";
import { __resetDirsFromEnvForTests, logger, setAgentDir, TempDir } from "@oh-my-pi/pi-utils";
import { mockSchedulerWaitWithClock } from "./helpers/mock-scheduler-clock";

import {
	cfgClaudeResetsAutoRedeem5h,
	cfgClaudeResetsAutoRedeemWeekly,
	cfgClaudeResetsApprovedRedeems,
	type ClaudeResetApproval,
	resetWindowModes,
} from "@oh-my-pi/pi-coding-agent/session/settings";

const ACCOUNT_ID = "claude-account";
const EMAIL = "claude@example.com";
const ORG_ID = "11111111-1111-4111-8111-111111111111";
const CREDENTIAL_ID = 7;
const HOUR = 3_600_000;
const CLAUDE_USAGE_LIMIT_ERROR =
	'429 {"type":"error","error":{"type":"rate_limit_error","message":"usage_limit_reached"}} retry-after-ms=259200000';

function claudeReport(weeklyUsed: number): UsageReport {
	const now = Date.now();
	return {
		provider: "anthropic",
		fetchedAt: now,
		limits: [
			{
				id: "anthropic:5h",
				label: "Claude 5 Hour",
				scope: { provider: "anthropic", shared: true, windowId: "5h" },
				window: { id: "5h", label: "5 Hour", durationMs: 5 * HOUR, resetsAt: now + 2 * HOUR },
				amount: { usedFraction: 0.5, unit: "percent" },
			},
			{
				id: "anthropic:7d",
				label: "Claude 7 Day",
				scope: { provider: "anthropic", shared: true, windowId: "7d" },
				window: { id: "7d", label: "7 Day", durationMs: 7 * 24 * HOUR, resetsAt: now + 3 * 24 * HOUR },
				amount: { usedFraction: weeklyUsed, unit: "percent" },
			},
		],
		metadata: { accountId: ACCOUNT_ID, email: EMAIL, orgId: ORG_ID },
	};
}

/** An operator approval covering the 7d window `claudeReport` blocks, against its banked count of one. */
function claudeApproval(): ClaudeResetApproval {
	return {
		accountId: ACCOUNT_ID,
		approvedBy: "matt",
		ref: "RIG-11726",
		expiresAt: new Date(Date.now() + 24 * HOUR).toISOString(),
		windowResetsAt: new Date(Date.now() + 3 * 24 * HOUR).toISOString(),
		creditsLeft: 1,
	};
}

/**
 * An operator approval for the outside account (`claude-excluded`) whose 7d window resets in `outsideWaitMs`.
 * The outside fixture banks two credits (`availableCount: 2`), and the guard matches `creditsLeft` to that count.
 */
function outsideApproval(outsideWaitMs: number): ClaudeResetApproval {
	return {
		accountId: "claude-excluded",
		approvedBy: "matt",
		ref: "RIG-11726",
		expiresAt: new Date(Date.now() + 24 * HOUR).toISOString(),
		windowResetsAt: new Date(Date.now() + outsideWaitMs).toISOString(),
		creditsLeft: 2,
	};
}

function claudeStatus(requiresLimit: boolean): ResetCreditAccountStatus {
	const expiresAt = new Date(Date.now() + 2 * HOUR).toISOString();
	return {
		provider: "anthropic",
		credentialId: CREDENTIAL_ID,
		report: claudeReport(requiresLimit ? 1 : 0.5),
		accountId: ACCOUNT_ID,
		email: EMAIL,
		orgId: ORG_ID,
		active: true,
		availableCount: 1,
		redeemableCount: 1,
		eligible: true,
		nextCreditId: "cedar-grant-1",
		credits: [
			{
				id: "cedar-grant-1",
				title: "Claude saved reset",
				program: "cedar_ember",
				remainingCount: 1,
				usable: true,
				requiresLimit,
				clears: ["anthropic:7d"],
				blocking: requiresLimit ? ["anthropic:7d"] : [],
				usedFractions: { "anthropic:7d": requiresLimit ? 1 : 0.5 },
				expiresAt,
				status: "available",
			},
		],
	};
}

/** The usage report as the broker or local usage fetch hands it over, carrying the account's reset inventory. */
function withInventory(report: UsageReport, status: ResetCreditAccountStatus): UsageReport {
	const { availableCount, redeemableCount, eligible, nextCreditId, credits } = status;
	return { ...report, resetCredits: { availableCount, redeemableCount, eligible, nextCreditId, credits } };
}

describe("Claude saved-reset trigger integration", () => {
	let authStorage: AuthStorage;
	let modelRegistry: ModelRegistry;
	let sessions: AgentSession[];
	let managers: SessionManager[];
	let tempDir: TempDir;

	beforeAll(async () => {
		authStorage = await AuthStorage.create(":memory:");
		modelRegistry = new ModelRegistry(authStorage, undefined, { ignoreLocalModelConfig: true });
	});

	beforeEach(() => {
		vi.spyOn(envApiKey, "getEnvApiKey").mockReturnValue(undefined);
		sessions = [];
		managers = [];
		tempDir = TempDir.createSync("@pi-claude-reset-");
	});

	afterEach(async () => {
		for (const session of sessions.splice(0).reverse()) {
			await session.dispose();
		}
		for (const manager of managers.splice(0).reverse()) {
			await manager.close();
		}
		tempDir.removeSync();
		vi.restoreAllMocks();
	});

	afterAll(() => {
		authStorage.close();
	});

	function buildSession(options: {
		withExtensionRunner?: boolean;
		report: UsageReport | null;
		status: ResetCreditAccountStatus;
		streamErrorFirst?: boolean;
		transientFailures?: number;
		listFailures?: number;
		maxDelayMs?: number;
		quota?: { restored: boolean };
		autoRedeem?: "unset" | "yes" | "no";
		salvageHorizonHours?: number;
		clearResetCredentialBlocks?: boolean;
		restoreWeeklyUsage?: boolean;
		useStoredCredentials?: boolean;
		keepCredits?: number;
		/** Answers the auto-redeem consent prompt; without it the session has no prompt UI. */
		consent?: (question: string) => Promise<string | undefined>;
		/** Shares another session's coordinator, as sessions of one process do. */
		coordinator?: CodexAutoRedeemCoordinator;
		/** Operator-approved Claude redeems; without one an automated Claude redeem is refused. */
		approvals?: readonly ClaudeResetApproval[];
	}): {
		session: AgentSession;
		coordinator: CodexAutoRedeemCoordinator;
		targets: ResetCreditTarget[];
		modelCalls: () => number;
		listCalls: () => number;
		resolvedApiKeys: string[];
	} {
		const model = getBundledModel("anthropic", "claude-sonnet-4-5");
		if (!model) throw new Error("Expected bundled anthropic/claude-sonnet-4-5 to exist");
		const resolvedApiKeys: string[] = [];
		if (options.useStoredCredentials) authStorage.keys.removeRuntime("anthropic");
		else {
			authStorage.keys.setRuntime("anthropic", "test-key");
			vi.spyOn(authStorage.oauth, "identity").mockReturnValue({
				accountId: ACCOUNT_ID,
				email: EMAIL,
				orgId: ORG_ID,
			});
			vi.spyOn(authStorage.oauth, "accounts").mockReturnValue([
				{
					position: 0,
					credentialId: CREDENTIAL_ID,
					accountId: ACCOUNT_ID,
					email: EMAIL,
					orgId: ORG_ID,
					active: true,
				},
			]);
		}
		vi.spyOn(authStorage.usage, "reports").mockImplementation(async () => options.report && [options.report]);
		let listAttempts = 0;
		vi.spyOn(authStorage.resets, "list").mockImplementation(async request => {
			if (request?.provider !== "anthropic") return [];
			listAttempts++;
			return [
				listAttempts <= (options.listFailures ?? 0)
					? { ...options.status, report: undefined, error: "Rate limited", retryAfterMs: 0 }
					: options.status,
			];
		});
		const targets: ResetCreditTarget[] = [];
		const quota = options.quota ?? { restored: !options.streamErrorFirst };
		vi.spyOn(authStorage.resets, "redeem").mockImplementation(async request => {
			targets.push(request.target);
			quota.restored = true;
			if (options.clearResetCredentialBlocks && options.status.credentialId !== undefined) {
				authStorage.blocks.deleteAll(options.status.credentialId);
			}
			if (options.restoreWeeklyUsage) {
				const weeklyLimit = options.report?.limits.find(limit => limit.id === "anthropic:7d");
				if (weeklyLimit?.amount) weeklyLimit.amount.usedFraction = 0;
			}
			return {
				ok: true,
				code: "reset",
				provider: "anthropic",
				accountId: ACCOUNT_ID,
				email: EMAIL,
				orgId: ORG_ID,
				creditId: "cedar-grant-1",
				cleared: ["anthropic:7d"],
			};
		});

		const mock = createMockModel();
		let calls = 0;
		const agent = new Agent({
			getApiKey: requestedModel => {
				if (!options.useStoredCredentials) return "test-key";
				const resolver = modelRegistry.resolver(requestedModel, agent.sessionId);
				return async context => {
					const resolved = await resolver(context);
					const apiKey =
						typeof resolved === "string"
							? resolved
							: resolved && "apiKey" in resolved
								? resolved.apiKey
								: undefined;
					if (apiKey) resolvedApiKeys.push(apiKey);
					return resolved;
				};
			},
			initialState: { model, systemPrompt: ["Test"], tools: [], messages: [] },
			streamFn: (requestedModel, context, streamOptions) => {
				calls++;
				const transientFailures = options.transientFailures ?? 0;
				if (calls <= transientFailures) mock.push({ throw: "503 Service unavailable" });
				else if (options.streamErrorFirst && (calls === transientFailures + 1 || !quota.restored)) {
					mock.push({ throw: CLAUDE_USAGE_LIMIT_ERROR });
				} else mock.push({ content: ["recovered after Claude reset"], stopReason: "stop" });
				return mock.stream(requestedModel, context, streamOptions);
			},
		});
		const settings = Settings.isolated({
			"compaction.enabled": false,
			"retry.baseDelayMs": 5,
			"retry.maxDelayMs": options.maxDelayMs ?? 100,
			"retry.maxRetries": 1,
			"codexResets.autoRedeem": "no",
			"claudeResets.autoRedeem": options.autoRedeem ?? "yes",
			"claudeResets.salvageHorizonHours": options.salvageHorizonHours ?? 12,
			"claudeResets.keepCredits": options.keepCredits ?? 0,
		});
		if (options.approvals) cfgClaudeResetsApprovedRedeems.set(settings, [...options.approvals]);
		settings.setModelRole("default", `${model.provider}/${model.id}`);
		const sessionManager = SessionManager.inMemory();
		managers.push(sessionManager);
		let coordinator = options.coordinator;
		if (!coordinator) {
			coordinator = createCodexAutoRedeemCoordinator();
			coordinator.resetLockPath = `${tempDir.path()}/auth.db`;
		}
		let extensionRunner: ExtensionRunner | undefined;
		if (options.consent) {
			extensionRunner = new ExtensionRunner(
				[],
				new ExtensionRuntime(),
				tempDir.path(),
				sessionManager,
				modelRegistry,
			);
			vi.spyOn(extensionRunner, "hasUI").mockReturnValue(true);
			vi.spyOn(extensionRunner.getUIContext(), "select").mockImplementation(options.consent);
		} else if (options.withExtensionRunner) {
			extensionRunner = new ExtensionRunner(
				[],
				new ExtensionRuntime(),
				tempDir.path(),
				sessionManager,
				modelRegistry,
			);
		}
		const session = new AgentSession({
			agent,
			sessionManager,
			settings,
			modelRegistry,
			codexResetCoordinator: coordinator,
			extensionRunner,
		});
		sessions.push(session);
		return { session, coordinator, targets, modelCalls: () => calls, listCalls: () => listAttempts, resolvedApiKeys };
	}

	/**
	 * Stores the session's account and one outside its pool, pools the session to
	 * its own, and lists both as blocked with a saved reset. The outside account's
	 * weekly wall clears in `outsideWaitMs` and its reset is far from expiry, so
	 * only a restore could spend it.
	 */
	async function poolToSessionAccount(
		session: AgentSession,
		outsideWaitMs: number,
	): Promise<{ pooledId: number; outsideId: number; release: () => Promise<void> }> {
		await authStorage.credentials.set(
			"anthropic",
			[
				{ accountId: ACCOUNT_ID, email: EMAIL, orgId: ORG_ID },
				{ accountId: "claude-excluded", email: "excluded@example.com", orgId: "org-excluded" },
			].map(identity => ({
				type: "oauth" as const,
				access: `access-${identity.accountId}`,
				refresh: `refresh-${identity.accountId}`,
				expires: Date.now() + HOUR,
				...identity,
			})),
		);
		const [pooled, outside] = authStorage.credentials.list("anthropic");
		if (!pooled || !outside) throw new Error("expected stored accounts");
		const outsideReport = claudeReport(1);
		outsideReport.metadata = { accountId: "claude-excluded", email: "excluded@example.com", orgId: "org-excluded" };
		for (const limit of outsideReport.limits) {
			if (limit.id === "anthropic:7d" && limit.window) limit.window.resetsAt = Date.now() + outsideWaitMs;
		}
		const outsideStatus: ResetCreditAccountStatus = {
			...claudeStatus(true),
			credentialId: outside.id,
			accountId: "claude-excluded",
			email: "excluded@example.com",
			orgId: "org-excluded",
			active: false,
			availableCount: 2,
			report: outsideReport,
		};
		outsideStatus.credits = outsideStatus.credits.map(credit => ({
			...credit,
			expiresAt: new Date(Date.now() + 20 * 24 * HOUR).toISOString(),
		}));
		vi.spyOn(authStorage.resets, "list").mockImplementation(async () => [
			{ ...claudeStatus(true), credentialId: pooled.id },
			outsideStatus,
		]);
		const lease = authStorage.sessions.restrict("anthropic", session.sessionId, [`email:${EMAIL}|org:${ORG_ID}`]);
		return {
			pooledId: pooled.id,
			outsideId: outside.id,
			release: async () => {
				authStorage.sessions.unrestrict("anthropic", session.sessionId, lease);
				await authStorage.credentials.remove("anthropic");
			},
		};
	}

	/** Records each redeemed credential and restores the quota it serves. */
	function recordSpends(quotaFor: (credentialId: number) => { restored: boolean } | undefined): number[] {
		const spent: number[] = [];
		vi.spyOn(authStorage.resets, "redeem").mockImplementation(async request => {
			spent.push(request.target.credentialId);
			const quota = quotaFor(request.target.credentialId);
			if (quota) quota.restored = true;
			return {
				ok: true,
				code: "reset",
				provider: "anthropic",
				creditId: "cedar-grant-1",
				cleared: ["anthropic:7d"],
			};
		});
		return spent;
	}

	/**
	 * Prompts `runner` and, once its blocked pass is listing resets, `joiner`,
	 * which finds that pass in flight; the listing answers only after it has.
	 */
	async function promptJoined(
		runner: AgentSession,
		joiner: AgentSession,
		coordinator: CodexAutoRedeemCoordinator,
	): Promise<void> {
		const statuses = await authStorage.resets.list({ provider: "anthropic" });
		const listing = Promise.withResolvers<void>();
		const joined = Promise.withResolvers<void>();
		vi.spyOn(authStorage.resets, "list").mockImplementation(async () => {
			listing.resolve();
			await joined.promise;
			return statuses;
		});
		const inFlight = coordinator.inFlightByAccount;
		const get = inFlight.get.bind(inFlight);
		vi.spyOn(inFlight, "get").mockImplementation(key => {
			const pass = get(key);
			if (pass) joined.resolve();
			return pass;
		});
		const running = runner.prompt("run the blocked pass");
		await listing.promise;
		await Promise.all([running, joiner.prompt("join the blocked pass")]);
		await Promise.all([runner.waitForIdle(), joiner.waitForIdle()]);
	}

	/** Empties the session's pool while the first pending attempt marker is written; returns that lease. */
	function emptyPoolOnPendingMarker(session: AgentSession): () => SessionRestrictionLease | undefined {
		const write = Bun.write.bind(Bun);
		let replaced: SessionRestrictionLease | undefined;
		vi.spyOn(Bun, "write").mockImplementation(async (destination, input) => {
			// Only the reset lock files are written here, always as text.
			const written = await write(destination as string, input as string);
			if (!replaced && typeof input === "string" && input.startsWith("pending:")) {
				replaced = authStorage.sessions.restrict("anthropic", session.sessionId, []);
			}
			return written;
		});
		return () => replaced;
	}

	it("redeems the exact live Cedar grant on a blocked retry and immediately recovers", async () => {
		const { session, targets } = buildSession({
			report: claudeReport(1),
			status: claudeStatus(true),
			streamErrorFirst: true,
			approvals: [claudeApproval()],
		});
		mockSchedulerWaitWithClock();

		await session.prompt("trigger a Claude usage limit");
		await session.waitForIdle();

		expect(targets).toEqual([
			{
				provider: "anthropic",
				credentialId: CREDENTIAL_ID,
				creditId: "cedar-grant-1",
				accountId: ACCOUNT_ID,
				email: EMAIL,
				orgId: ORG_ID,
			},
		]);
		const recovered = session.sessionManager
			.getEntries()
			.some(
				entry =>
					entry.type === "message" &&
					entry.message.role === "assistant" &&
					entry.message.content.some(
						block => block.type === "text" && block.text === "recovered after Claude reset",
					),
			);
		expect(recovered).toBe(true);
	});

	it("does not redeem a Claude reset in a session without an approval record", async () => {
		const { session, targets } = buildSession({
			report: claudeReport(1),
			status: claudeStatus(true),
			streamErrorFirst: true,
		});
		mockSchedulerWaitWithClock();

		await session.prompt("trigger a Claude usage limit");
		await session.waitForIdle();

		expect(targets).toEqual([]);
	});

	it("redeems a banked reset before retrying with OAuth when an unblocked stored API key exists", async () => {
		const oauthAccess = "claude-oauth-access-token";
		const storedApiKey = "sk-ant-api03-test";
		const credentials: AuthCredential[] = [
			{
				type: "oauth",
				access: oauthAccess,
				refresh: "claude-oauth-refresh-token",
				expires: Date.now() + HOUR,
				accountId: ACCOUNT_ID,
				email: EMAIL,
				orgId: ORG_ID,
			},
			{ type: "api_key", key: storedApiKey },
		];
		await authStorage.credentials.set("anthropic", credentials);
		const oauthEntry = authStorage.credentials.list("anthropic").find(entry => entry.credential.type === "oauth");
		if (!oauthEntry) throw new Error("Expected stored Claude OAuth credential");
		const status = { ...claudeStatus(true), credentialId: oauthEntry.id };
		const { session, targets, modelCalls, resolvedApiKeys } = buildSession({
			autoRedeem: "yes",
			report: claudeReport(1),
			status,
			streamErrorFirst: true,
			useStoredCredentials: true,
			clearResetCredentialBlocks: true,
			restoreWeeklyUsage: true,
			approvals: [claudeApproval()],
		});
		mockSchedulerWaitWithClock();

		try {
			await session.prompt("redeem the banked reset before spending Console credit");
			await session.waitForIdle();
		} finally {
			await authStorage.credentials.remove("anthropic");
		}

		expect(targets).toEqual([
			{
				provider: "anthropic",
				credentialId: oauthEntry.id,
				creditId: "cedar-grant-1",
				accountId: ACCOUNT_ID,
				email: EMAIL,
				orgId: ORG_ID,
			},
		]);
		expect(modelCalls()).toBe(2);
		expect(resolvedApiKeys).toEqual([oauthAccess, oauthAccess]);
		expect(resolvedApiKeys).not.toContain(storedApiKey);
	});

	it("continues the task using live reset evidence when broker usage polling is unavailable", async () => {
		const { session, targets } = buildSession({
			report: null,
			status: claudeStatus(true),
			streamErrorFirst: true,
			approvals: [claudeApproval()],
		});

		await session.prompt("continue through a quota reset");
		await session.waitForIdle();

		expect(targets.map(target => target.credentialId)).toEqual([CREDENTIAL_ID]);
		expect(session.agent.state.messages.at(-1)).toMatchObject({
			role: "assistant",
			stopReason: "stop",
			content: [{ type: "text", text: "recovered after Claude reset" }],
		});
	});

	it("redeems and continues when earlier provider failures consumed the retry budget", async () => {
		const { session, targets } = buildSession({
			report: null,
			status: claudeStatus(true),
			streamErrorFirst: true,
			transientFailures: 1,
			approvals: [claudeApproval()],
		});
		mockSchedulerWaitWithClock();

		await session.prompt("recover after retry budget exhaustion");
		await session.waitForIdle();

		expect(targets.map(target => target.credentialId)).toEqual([CREDENTIAL_ID]);
		expect(session.agent.state.messages.at(-1)).toMatchObject({ role: "assistant", stopReason: "stop" });
	});

	it("waits for throttled reset discovery and resumes without retrying the blocked model prematurely", async () => {
		const { session, targets } = buildSession({
			report: null,
			status: claudeStatus(true),
			streamErrorFirst: true,
			listFailures: 1,
			maxDelayMs: 2_500,
			approvals: [claudeApproval()],
		});
		mockSchedulerWaitWithClock();

		await session.prompt("wait for reset eligibility");
		await session.waitForIdle();

		expect(targets.map(target => target.credentialId)).toEqual([CREDENTIAL_ID]);
		expect(session.agent.state.messages.at(-1)).toMatchObject({ role: "assistant", stopReason: "stop" });
	});

	it("never spends the reset of an account outside the session's account pool", async () => {
		// The pooled account holds only the kept reserve; the excluded one has a spare reset.
		const { session, targets } = buildSession({
			report: null,
			status: claudeStatus(true),
			streamErrorFirst: true,
			keepCredits: 1,
		});
		const pool = await poolToSessionAccount(session, 3 * 24 * HOUR);
		mockSchedulerWaitWithClock();

		try {
			await session.prompt("stay inside the account pool");
			await session.waitForIdle();
		} finally {
			await pool.release();
		}

		expect(targets).toEqual([]);
		expect(session.agent.state.messages.at(-1)).toMatchObject({ role: "assistant", stopReason: "error" });
	});

	it("drops a planned restore whose account leaves the session's pool before it is spent", async () => {
		let replaced: SessionRestrictionLease | undefined;
		const { session, coordinator, targets } = buildSession({
			report: null,
			status: claudeStatus(true),
			streamErrorFirst: true,
			autoRedeem: "unset",
			approvals: [claudeApproval()],
			// The pool is replaced while the planned restore waits for consent.
			consent: async () => {
				replaced = authStorage.sessions.restrict("anthropic", session.sessionId, []);
				return "Yes";
			},
		});
		const pool = await poolToSessionAccount(session, 3 * 24 * HOUR);
		mockSchedulerWaitWithClock();

		try {
			await session.prompt("lose the pooled account mid-recovery");
			await session.waitForIdle();
		} finally {
			if (replaced) authStorage.sessions.unrestrict("anthropic", session.sessionId, replaced);
			await pool.release();
		}

		expect(replaced).toBeDefined();
		expect(targets).toEqual([]);
		expect(coordinator.attemptedKeys.size).toBe(0);
		expect(session.agent.state.messages.at(-1)).toMatchObject({ role: "assistant", stopReason: "error" });
	});

	it("drops a restore whose account leaves the session's pool while its attempt is recorded, then retries it", async () => {
		const { session, coordinator, targets } = buildSession({
			report: null,
			status: claudeStatus(true),
			streamErrorFirst: true,
			approvals: [claudeApproval()],
		});
		const pool = await poolToSessionAccount(session, 3 * 24 * HOUR);
		mockSchedulerWaitWithClock();
		const replaced = emptyPoolOnPendingMarker(session);
		let repooled: SessionRestrictionLease | undefined;
		try {
			await session.prompt("lose the pooled account while recording the attempt");
			await session.waitForIdle();
			expect(replaced()).toBeDefined();
			expect(targets).toEqual([]);
			expect(coordinator.attemptedKeys.size).toBe(0);
			expect(session.agent.state.messages.at(-1)).toMatchObject({ role: "assistant", stopReason: "error" });

			// Neither the attempt nor its pending marker fences the account once it is pooled again.
			repooled = authStorage.sessions.restrict("anthropic", session.sessionId, [`email:${EMAIL}|org:${ORG_ID}`]);
			await session.prompt("recover once the account is pooled again");
			await session.waitForIdle();
		} finally {
			const lease = repooled ?? replaced();
			if (lease) authStorage.sessions.unrestrict("anthropic", session.sessionId, lease);
			await pool.release();
		}

		expect(targets.map(target => target.credentialId)).toEqual([pool.pooledId]);
		expect(session.agent.state.messages.at(-1)).toMatchObject({ role: "assistant", stopReason: "stop" });
	});

	it("restores an account outside a pooled peer's pool for an unrestricted session that joined its pass", async () => {
		// The shared account holds only the kept reserve; the outside account has a spare reset.
		const openQuota = { restored: false };
		const pooled = buildSession({
			report: null,
			status: claudeStatus(true),
			streamErrorFirst: true,
			keepCredits: 1,
			approvals: [claudeApproval()],
		});
		const open = buildSession({
			report: null,
			status: claudeStatus(true),
			streamErrorFirst: true,
			keepCredits: 1,
			quota: openQuota,
			coordinator: pooled.coordinator,
			approvals: [outsideApproval(3 * 24 * HOUR)],
		});
		const pool = await poolToSessionAccount(pooled.session, 3 * 24 * HOUR);
		const spent = recordSpends(credentialId => (credentialId === pool.outsideId ? openQuota : undefined));
		mockSchedulerWaitWithClock();

		try {
			await promptJoined(pooled.session, open.session, pooled.coordinator);
		} finally {
			await pool.release();
		}

		expect(spent).toEqual([pool.outsideId]);
		expect(open.session.agent.state.messages.at(-1)).toMatchObject({ role: "assistant", stopReason: "stop" });
		expect(pooled.session.agent.state.messages.at(-1)).toMatchObject({ role: "assistant", stopReason: "error" });
	});

	it("restores the shared account for an unrestricted session that joined a pass whose pool dropped it mid-spend", async () => {
		const openQuota = { restored: false };
		const pooled = buildSession({
			report: null,
			status: claudeStatus(true),
			streamErrorFirst: true,
			approvals: [claudeApproval()],
		});
		const open = buildSession({
			report: null,
			status: claudeStatus(true),
			streamErrorFirst: true,
			quota: openQuota,
			coordinator: pooled.coordinator,
			approvals: [claudeApproval()],
		});
		const pool = await poolToSessionAccount(pooled.session, 3 * 24 * HOUR);
		// Only the shared account is listed, so planning excludes nothing.
		const listed = await authStorage.resets.list({ provider: "anthropic" });
		vi.spyOn(authStorage.resets, "list").mockResolvedValue(
			listed.filter(status => status.credentialId === pool.pooledId),
		);
		const replaced = emptyPoolOnPendingMarker(pooled.session);
		const spent = recordSpends(credentialId => (credentialId === pool.pooledId ? openQuota : undefined));
		mockSchedulerWaitWithClock();

		try {
			await promptJoined(pooled.session, open.session, pooled.coordinator);
		} finally {
			const lease = replaced();
			if (lease) authStorage.sessions.unrestrict("anthropic", pooled.session.sessionId, lease);
			await pool.release();
		}

		expect(replaced()).toBeDefined();
		expect(spent).toEqual([pool.pooledId]);
		expect(open.modelCalls()).toBe(2);
		expect(open.session.agent.state.messages.at(-1)).toMatchObject({ role: "assistant", stopReason: "stop" });
		expect(pooled.session.agent.state.messages.at(-1)).toMatchObject({ role: "assistant", stopReason: "error" });
	});

	it("recovers a pooled session and an unrestricted one with one restore of the account they share", async () => {
		const quota = { restored: false };
		const open = buildSession({
			report: null,
			status: claudeStatus(true),
			streamErrorFirst: true,
			quota,
			approvals: [claudeApproval()],
		});
		const pooled = buildSession({
			report: null,
			status: claudeStatus(true),
			streamErrorFirst: true,
			quota,
			coordinator: open.coordinator,
			approvals: [claudeApproval()],
		});
		const pool = await poolToSessionAccount(pooled.session, 24 * HOUR);
		const spent = recordSpends(credentialId => (credentialId === pool.pooledId ? quota : undefined));
		mockSchedulerWaitWithClock();

		try {
			await promptJoined(open.session, pooled.session, open.coordinator);
		} finally {
			await pool.release();
		}

		expect(spent).toEqual([pool.pooledId]);
		for (const { session, modelCalls } of [open, pooled]) {
			expect(modelCalls()).toBe(2);
			expect(session.agent.state.messages.at(-1)).toMatchObject({ role: "assistant", stopReason: "stop" });
		}
	});

	it("cancels reset discovery backoff without spending a credit or resuming the task", async () => {
		const { session, targets } = buildSession({
			report: null,
			status: claudeStatus(true),
			streamErrorFirst: true,
			maxDelayMs: 5_000,
		});
		const listed = Promise.withResolvers<void>();
		vi.spyOn(authStorage.resets, "list").mockImplementation(async () => {
			listed.resolve();
			return [{ ...claudeStatus(true), report: undefined, error: "Rate limited", retryAfterMs: 1_000 }];
		});

		const prompt = session.prompt("cancel while waiting for reset eligibility");
		await listed.promise;
		await session.abort();
		await prompt;
		await session.waitForIdle();

		expect(targets).toEqual([]);
		expect(session.isRetrying).toBe(false);
		expect(session.agent.state.messages.at(-1)).not.toMatchObject({ stopReason: "stop" });
	});

	it("adopts a peer's confirmed reset without spending again or looping on the same marker", async () => {
		const quota = { restored: false };
		const first = buildSession({
			report: null,
			status: claudeStatus(true),
			streamErrorFirst: true,
			quota,
			approvals: [claudeApproval()],
		});
		await first.session.prompt("restore the shared account");
		await first.session.waitForIdle();
		expect(first.targets).toHaveLength(1);

		const spent = claudeStatus(true);
		spent.availableCount = 0;
		spent.redeemableCount = 0;
		spent.eligible = false;
		const peer = buildSession({ report: null, status: spent, streamErrorFirst: true, quota });
		await peer.session.prompt("recover a request issued before the peer reset");
		await peer.session.waitForIdle();
		expect(peer.targets).toEqual([]);
		expect(peer.session.agent.state.messages.at(-1)).toMatchObject({ role: "assistant", stopReason: "stop" });

		quota.restored = false;
		await peer.session.prompt("do not reuse an old reset for a new quota failure");
		await peer.session.waitForIdle();
		expect(peer.targets).toEqual([]);
		expect(peer.session.agent.state.messages.at(-1)).toMatchObject({ role: "assistant", stopReason: "error" });
	});

	it("does not adopt a peer's reset of an account outside the session's pool", async () => {
		// The shared account holds only the kept reserve, so the unrestricted peer restores the outside account.
		const openQuota = { restored: false };
		const open = buildSession({
			report: null,
			status: claudeStatus(true),
			streamErrorFirst: true,
			keepCredits: 1,
			quota: openQuota,
			approvals: [outsideApproval(3 * 24 * HOUR)],
		});
		const pooled = buildSession({ report: null, status: claudeStatus(true), streamErrorFirst: true, keepCredits: 1 });
		const pool = await poolToSessionAccount(pooled.session, 3 * 24 * HOUR);
		const spent = recordSpends(credentialId => (credentialId === pool.outsideId ? openQuota : undefined));
		mockSchedulerWaitWithClock();

		try {
			await open.session.prompt("restore the outside account");
			await open.session.waitForIdle();
			await pooled.session.prompt("nothing in the pool can be restored");
			await pooled.session.waitForIdle();
		} finally {
			await pool.release();
		}

		expect(spent).toEqual([pool.outsideId]);
		expect(open.session.agent.state.messages.at(-1)).toMatchObject({ role: "assistant", stopReason: "stop" });
		// No retry of the still-blocked pooled account on the outside account's reset marker.
		expect(pooled.modelCalls()).toBe(1);
		expect(pooled.session.agent.state.messages.at(-1)).toMatchObject({ role: "assistant", stopReason: "error" });
	});

	it("adopts a reset `omp usage reset` just spent instead of spending again", async () => {
		const originalEnv = {
			PI_CODING_AGENT_DIR: process.env.PI_CODING_AGENT_DIR,
			OMP_PROFILE: process.env.OMP_PROFILE,
			PI_PROFILE: process.env.PI_PROFILE,
		};
		setAgentDir(tempDir.path());
		try {
			const status = claudeStatus(true);
			const { session, coordinator, targets } = buildSession({ report: null, status, streamErrorFirst: true });
			// The production lock root beside agent.db, which the CLI shares.
			coordinator.resetLockPath = undefined;
			vi.spyOn(Settings, "loadReadOnly").mockResolvedValue(Settings.isolated());
			vi.spyOn(sdkModule, "discoverAuthStorage").mockResolvedValue(authStorage);
			// The CLI closes the store it discovered; this one outlives the test.
			vi.spyOn(authStorage, "close").mockImplementation(() => {});
			let stdout = "";
			vi.spyOn(process.stdout, "write").mockImplementation(chunk => {
				stdout += String(chunk);
				return true;
			});
			await runUsageCommand({ action: "reset", target: `claude/${CREDENTIAL_ID}`, noExtensions: true });
			expect(stdout).toContain("Reset applied");
			expect(targets).toHaveLength(1);

			status.availableCount = 0;
			status.redeemableCount = 0;
			status.eligible = false;
			const revalidate = vi.spyOn(authStorage.credentials, "revalidate");
			await session.prompt("recover a request issued before the CLI reset");
			await session.waitForIdle();

			expect(targets).toHaveLength(1);
			expect(revalidate).toHaveBeenCalled();
			expect(session.agent.state.messages.at(-1)).toMatchObject({ role: "assistant", stopReason: "stop" });
		} finally {
			for (const [key, value] of Object.entries(originalEnv)) {
				if (value === undefined) delete process.env[key];
				else process.env[key] = value;
			}
			__resetDirsFromEnvForTests();
			// The CLI's ModelRegistry opened `<agentDir>/models.db`; Windows cannot delete it while open.
			closeModelCache();
		}
	});

	it("refuses an expiring Cedar grant salvage without an approval: a salvage spend has no blocked reset to bind", async () => {
		const status = claudeStatus(false);
		const { session, coordinator, targets } = buildSession({
			report: withInventory(claudeReport(0.5), status),
			status,
		});

		await session.fetchUsageReports();
		expect(coordinator.sweepPromise).toBeDefined();
		await coordinator.sweepPromise;
		expect(targets).toEqual([]);

		coordinator.lastSweepAt = 0;
		await session.fetchUsageReports();
		await coordinator.sweepPromise;
		expect(targets).toEqual([]);
	});

	it("lists no Claude account and salvages nothing when its usage report carries no reset inventory", async () => {
		const { session, coordinator, targets, listCalls } = buildSession({
			report: claudeReport(0.5),
			status: claudeStatus(false),
		});

		await session.fetchUsageReports();
		await coordinator.sweepPromise;
		expect(targets).toEqual([]);
		expect(listCalls()).toBe(0);
	});

	it("does not salvage on usage a carried-over inventory recorded before the window rolled over", async () => {
		// A failed reset probe keeps the previous report's inventory, usage figures included.
		const carried = claudeStatus(false);
		carried.credits[0]!.usedFractions = { "anthropic:7d": 0.8 };
		const live = claudeStatus(false);
		live.report = claudeReport(0.01);
		live.credits[0]!.usedFractions = { "anthropic:7d": 0.01 };
		const { session, coordinator, targets, listCalls } = buildSession({
			report: withInventory(claudeReport(0.01), carried),
			status: live,
		});

		await session.fetchUsageReports();
		await coordinator.sweepPromise;
		expect(listCalls()).toBe(1);
		expect(targets).toEqual([]);
	});

	it("refuses a salvage for a credential stored without its organization: no approval can bind a salvage spend", async () => {
		const status = claudeStatus(false);
		const report = withInventory(claudeReport(0.5), status);
		report.metadata = { accountId: ACCOUNT_ID, email: EMAIL };
		const { session, coordinator, targets } = buildSession({ report, status });
		vi.spyOn(authStorage.oauth, "accounts").mockReturnValue([
			{ position: 0, credentialId: CREDENTIAL_ID, accountId: ACCOUNT_ID, email: EMAIL, active: true },
		]);

		await session.fetchUsageReports();
		await coordinator.sweepPromise;
		expect(targets).toEqual([]);
		expect([...coordinator.lastAttemptAtByAccount.keys()]).toEqual([]);
	});

	it.each(["yes", "no", "unset"] as const)(
		"refuses an imminent-reset salvage without an approval, for every consent value (%s)",
		async autoRedeem => {
			const status = claudeStatus(false);
			for (const credit of status.credits) {
				credit.expiresAt = new Date(Date.now() + 4 * 60_000).toISOString();
				credit.usedFractions = { "anthropic:7d": 0 };
			}
			const report = withInventory(claudeReport(0), status);
			status.report = report;
			const { session, coordinator, targets } = buildSession({
				report,
				status,
				autoRedeem,
				salvageHorizonHours: 0,
				keepCredits: 1,
			});

			await session.fetchUsageReports();
			await coordinator.sweepPromise;
			expect(targets).toEqual([]);

			coordinator.lastSweepAt = 0;
			await session.fetchUsageReports();
			await coordinator.sweepPromise;
			expect(targets).toEqual([]);
		},
	);

	it("warns once per attempt when a Claude redeem is refused for want of an approval, across sweeps", async () => {
		const warn = vi.spyOn(logger, "warn");
		const status = claudeStatus(false);
		for (const credit of status.credits) {
			credit.expiresAt = new Date(Date.now() + 4 * 60_000).toISOString();
			credit.usedFractions = { "anthropic:7d": 0 };
		}
		const report = withInventory(claudeReport(0), status);
		status.report = report;
		const { session, coordinator, targets } = buildSession({
			report,
			status,
			autoRedeem: "yes",
			salvageHorizonHours: 0,
			keepCredits: 1,
		});

		await session.fetchUsageReports();
		await coordinator.sweepPromise;
		coordinator.lastSweepAt = 0;
		await session.fetchUsageReports();
		await coordinator.sweepPromise;

		const refusals = warn.mock.calls.filter(
			([message]) => message === "claude-auto-reset: redeem refused, no matching approval record",
		);
		expect(refusals).toHaveLength(1);
		expect(targets).toEqual([]);
	});

	it("does not spend headlessly before independent Claude consent", async () => {
		// Codex being disabled does not enable Claude, and short of a reset about to expire an unset headless
		// session cannot spend silently.
		const status = claudeStatus(false);
		const { session, coordinator, targets } = buildSession({
			report: withInventory(claudeReport(0.5), status),
			status,
			autoRedeem: "unset",
		});

		await session.fetchUsageReports();
		await coordinator.sweepPromise;
		expect(targets).toHaveLength(0);
		expect(coordinator.attemptedKeys.size).toBe(0);
		expect(resetWindowModes("anthropic", session.settings)).toEqual({
			"5h": "unset",
			weekly: "unset",
			fable: "unset",
		});
	});

	it("offers an unset Claude reset to the prompt when an operator approval matches it", async () => {
		const questions: string[] = [];
		const { session, targets } = buildSession({
			report: null,
			status: claudeStatus(true),
			streamErrorFirst: true,
			autoRedeem: "unset",
			approvals: [claudeApproval()],
			consent: async question => {
				questions.push(question);
				return "Yes";
			},
		});
		mockSchedulerWaitWithClock();

		await session.prompt("ask about a reset an approval covers");
		await session.waitForIdle();

		expect(questions).toEqual([expect.stringContaining("Spend a saved Claude rate-limit reset?")]);
		expect(targets.map(target => target.credentialId)).toEqual([CREDENTIAL_ID]);
	});

	it("never asks about an imminent Claude salvage that no approval can match, even with a prompt UI", async () => {
		const status = claudeStatus(false);
		for (const credit of status.credits) {
			credit.expiresAt = new Date(Date.now() + 4 * 60_000).toISOString();
		}
		const { availableCount, redeemableCount, eligible, nextCreditId, credits } = status;
		const report = {
			...claudeReport(0.5),
			resetCredits: { availableCount, redeemableCount, eligible, nextCreditId, credits },
		};
		status.report = report;
		const { session, coordinator, targets } = buildSession({
			withExtensionRunner: true,
			report,
			status,
			autoRedeem: "unset",
			salvageHorizonHours: 0,
		});
		const questions: string[] = [];
		await initializeExtensions(session, {
			reportSendError: () => {},
			reportRuntimeError: () => {},
			uiContext: Object.create(session.extensionRunner!.getUIContext(), {
				select: {
					value: async (question: string) => {
						questions.push(question);
						return "No";
					},
				},
			}),
		});

		await session.fetchUsageReports();
		await coordinator.sweepPromise;
		expect(questions).toEqual([]);
		expect(targets).toEqual([]);
	});

	it("a Yes answer saves only the window key of the offered restore, not an unmatchable salvage's weekly key", async () => {
		// The pooled account is blocked on its 5h window only, so its restore is governed by the 5h key. The
		// outside account holds an expiring weekly Cedar grant that no approval can match, so it is never offered.
		const questions: string[] = [];
		const status = claudeStatus(true);
		const pooled = status.credits[0]!;
		pooled.clears = ["anthropic:5h"];
		pooled.blocking = ["anthropic:5h"];
		pooled.usedFractions = { "anthropic:5h": 1 };
		const report = claudeReport(0.5);
		for (const limit of report.limits) {
			if (limit.id === "anthropic:5h" && limit.amount) limit.amount.usedFraction = 1;
		}
		status.report = report;
		const { session, targets } = buildSession({
			report: null,
			status,
			streamErrorFirst: true,
			autoRedeem: "unset",
			approvals: [
				{
					accountId: ACCOUNT_ID,
					approvedBy: "matt",
					ref: "RIG-12027",
					expiresAt: new Date(Date.now() + 24 * HOUR).toISOString(),
					windowResetsAt: new Date(Date.now() + 2 * HOUR).toISOString(),
					creditsLeft: 1,
				},
			],
			consent: async question => {
				questions.push(question);
				return "Yes";
			},
		});
		// `autoRedeem: "unset"` migrates into runtime overrides that pin both window keys; clear them so the
		// consent save is observable.
		cfgClaudeResetsAutoRedeem5h.clearOverride(session.settings);
		cfgClaudeResetsAutoRedeemWeekly.clearOverride(session.settings);
		const outsideReport = claudeReport(0.5);
		outsideReport.metadata = { accountId: "claude-excluded", email: "excluded@example.com", orgId: "org-excluded" };
		const outside: ResetCreditAccountStatus = {
			...claudeStatus(false),
			credentialId: 8,
			accountId: "claude-excluded",
			email: "excluded@example.com",
			orgId: "org-excluded",
			active: false,
			report: outsideReport,
		};
		vi.spyOn(authStorage.resets, "list").mockImplementation(async request =>
			request?.provider === "anthropic" ? [status, outside] : [],
		);
		mockSchedulerWaitWithClock();

		await session.prompt("ask about the 5h restore while a weekly salvage is planned");
		await session.waitForIdle();

		expect(questions).toHaveLength(1);
		expect(resetWindowModes("anthropic", session.settings)).toMatchObject({ "5h": "yes", weekly: "unset" });
		expect(targets.map(target => target.credentialId)).toEqual([CREDENTIAL_ID]);
	});
});
