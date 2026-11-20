import type { Effort } from "@oh-my-pi/pi-catalog/effort";
import { THINKING_EFFORTS } from "@oh-my-pi/pi-catalog/effort";
import type { Api, Model } from "@oh-my-pi/pi-catalog/types";
import { logger } from "@oh-my-pi/pi-utils";
import type { ConfiguredThinkingLevel } from "@oh-my-pi/pi-tui/thinking";
import { AUTO_THINKING, resolveProvisionalAutoLevel } from "@oh-my-pi/pi-tui/thinking";
import { resolveModelRoleValue } from "../config/model-resolver";
import type { Settings } from "../config/settings";
import { cfgAutolearnCaptureRole } from "./settings";

/**
 * The model and thinking level the capture runs on, from `autolearn.captureRole`.
 *
 * - No role configured: `undefined`, so the capture keeps the seat's model.
 * - Role not configured, or its model unavailable: `null`, so the capture is skipped.
 *   It never falls back to the seat's model.
 * - Otherwise the role's model and its thinking level. `off` is kept as `off`, so the runner turns reasoning
 *   off. `auto` takes the role model's provisional level; a role that names no level takes `fallbackLevel`.
 */
export function resolveAutolearnCaptureTarget(
	settings: Settings,
	availableModels: Model<Api>[],
	fallbackLevel: Effort | undefined,
): { model: Model<Api>; thinkingLevel: Effort | "off" | undefined } | null | undefined {
	const role = cfgAutolearnCaptureRole.get(settings)?.trim();
	if (!role) return undefined;
	const roleValue = settings.getModelRole(role);
	if (!roleValue) {
		logger.warn("auto-learn capture skipped: autolearn.captureRole is not a configured role", { role });
		return null;
	}
	const resolved = resolveModelRoleValue(roleValue, availableModels, { settings });
	if (!resolved.model) {
		logger.warn("auto-learn capture skipped: autolearn.captureRole model is unavailable", {
			role,
			warning: resolved.warning,
		});
		return null;
	}
	return { model: resolved.model, thinkingLevel: roleLevel(resolved.model, resolved.thinkingLevel, fallbackLevel) };
}

/**
 * A role's level. `off` stays `off`. A concrete level passes through. `auto` takes the role model's provisional
 * level, as the session does (`resolveProvisionalAutoLevel`). A role that names no level takes `fallbackLevel`.
 */
function roleLevel(
	model: Model<Api>,
	level: ConfiguredThinkingLevel | undefined,
	fallbackLevel: Effort | undefined,
): Effort | "off" | undefined {
	if (level === "off") return "off";
	if (level === AUTO_THINKING) return resolveProvisionalAutoLevel(model) ?? fallbackLevel;
	return THINKING_EFFORTS.find(effort => effort === level) ?? fallbackLevel;
}
