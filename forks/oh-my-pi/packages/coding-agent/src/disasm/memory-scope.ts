/** Prefix the host argv with systemd-run when a Linux memory limit is available. */
export function withMemoryScope(
	cmd: readonly string[],
	memoryMax: string,
	systemdRun: string | undefined,
	platform: string,
): readonly string[] {
	if (platform !== "linux" || !memoryMax || !systemdRun) return cmd;
	return [
		systemdRun,
		"--user",
		"--scope",
		"--slice-inherit",
		"--quiet",
		"--collect",
		"-p",
		`MemoryMax=${memoryMax}`,
		"-p",
		"MemorySwapMax=0",
		"--",
		...cmd,
	];
}

let memoryScopeDisabled = false;
let passingProbe: Promise<boolean> | undefined;

/** Reset process-wide scope state between tests. */
export function resetMemoryScopeForTest(): void {
	memoryScopeDisabled = false;
	passingProbe = undefined;
}

export type ScopeSkipReason = "startup-failure" | "previous-failure" | "missing-systemd-run" | "disabled-by-setting";

export type HostStartupResult<T> =
	| { state: "started"; value: T }
	| { state: "contention" }
	| { state: "exited"; reason: string; readyAt?: number };

/** Start a host in scope, retrying once unscoped when systemd exits before readiness. */
export async function startWithMemoryScopeFallback<T>(
	cmd: readonly string[],
	memoryMax: string,
	systemdRun: string | undefined,
	platform: string,
	start: (command: readonly string[]) => Promise<HostStartupResult<T>>,
	onScopeSkipped: (reason: ScopeSkipReason) => void,
	probe: (systemdRun: string, memoryMax: string) => Promise<boolean> = probeMemoryScope,
): Promise<HostStartupResult<T>> {
	if (platform !== "linux") return start(cmd);
	if (!memoryMax) {
		onScopeSkipped("disabled-by-setting");
		return start(cmd);
	}
	if (!systemdRun) {
		onScopeSkipped("missing-systemd-run");
		return start(cmd);
	}
	if (memoryScopeDisabled) {
		onScopeSkipped("previous-failure");
		return start(cmd);
	}

	const result = await start(withMemoryScope(cmd, memoryMax, systemdRun, platform));
	if (result.state !== "exited" || result.readyAt !== undefined) return result;
	passingProbe ??= probe(systemdRun, memoryMax);
	if (await passingProbe) return result;
	passingProbe = undefined;

	memoryScopeDisabled = true;
	onScopeSkipped("startup-failure");
	return start(cmd);
}

async function probeMemoryScope(systemdRun: string, memoryMax: string): Promise<boolean> {
	try {
		const child = Bun.spawn([...withMemoryScope(["true"], memoryMax, systemdRun, "linux")], {
			stdout: "ignore",
			stderr: "ignore",
			timeout: 10_000,
		});
		return (await child.exited) === 0;
	} catch {
		return false;
	}
}
