import { logger } from "@oh-my-pi/pi-utils";
import type { ClaudeResetApproval } from "./settings";

/** Live reset times jitter by a few seconds, and an operator types a whole minute; match within this many ms. */
export const APPROVAL_RESET_TOLERANCE_MS = 5 * 60_000;

/** Stable identity of one approval: its account, the window it covers, and its ref. */
export function approvalSpentKey(approval: ClaudeResetApproval): string {
	return `${approval.accountId}@${approval.windowResetsAt}@${approval.ref}`;
}

/**
 * Whether an operator-typed approval entry is usable. Entries are typed by hand, so an entry that is not
 * an object, or lacks a non-empty `accountId`, a parseable `expiresAt`/`windowResetsAt`, or a
 * non-negative integer `creditsLeft`, can never match. It is skipped, with one warn per bad entry.
 */
export function validApprovalEntry(entry: unknown, warned: Set<string>): entry is ClaudeResetApproval {
	if (typeof entry === "object" && entry !== null) {
		const accountId = "accountId" in entry ? entry.accountId : undefined;
		const expiresAt = "expiresAt" in entry ? entry.expiresAt : undefined;
		const windowResetsAt = "windowResetsAt" in entry ? entry.windowResetsAt : undefined;
		const creditsLeft = "creditsLeft" in entry ? entry.creditsLeft : undefined;
		if (
			typeof accountId === "string" &&
			accountId.trim() !== "" &&
			typeof expiresAt === "string" &&
			Number.isFinite(Date.parse(expiresAt)) &&
			typeof windowResetsAt === "string" &&
			Number.isFinite(Date.parse(windowResetsAt)) &&
			typeof creditsLeft === "number" &&
			Number.isInteger(creditsLeft) &&
			creditsLeft >= 0
		) {
			return true;
		}
	}
	const key = JSON.stringify(entry) ?? String(entry);
	if (!warned.has(key)) {
		warned.add(key);
		logger.warn("auth-gateway Claude reset approval skipped", { reason: "malformed entry" });
	}
	return false;
}

/**
 * The approval that authorizes one Claude weekly redeem: same account, unexpired, bound to this window's
 * natural reset time, matched to the banked count it was given for, and not yet spent. Returns `undefined`
 * when none matches; every automated Claude redeem site refuses in that case.
 */
export function findClaudeResetApproval(args: {
	approvals: readonly unknown[];
	accountId: string | undefined;
	blockedResetsAt: number | undefined;
	bankedCount: number | undefined;
	spent: ReadonlySet<string>;
	warned: Set<string>;
	nowMs: number;
}): ClaudeResetApproval | undefined {
	if (args.accountId === undefined) return undefined;
	return args.approvals.find((approval): approval is ClaudeResetApproval => {
		if (!validApprovalEntry(approval, args.warned)) return false;
		return (
			approval.accountId === args.accountId &&
			Date.parse(approval.expiresAt) > args.nowMs &&
			args.blockedResetsAt !== undefined &&
			Math.abs(Date.parse(approval.windowResetsAt) - args.blockedResetsAt) <= APPROVAL_RESET_TOLERANCE_MS &&
			approval.creditsLeft === args.bankedCount &&
			!args.spent.has(approvalSpentKey(approval))
		);
	});
}
