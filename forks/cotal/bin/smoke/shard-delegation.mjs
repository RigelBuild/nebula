/** The only smoke suite whose runner requires a system-delegated cgroup. */
export const DELEGATED_SUITE = "smoke:manager-stop-reap";

/** @typedef {{ suite: string, shard: number, count: number, pinnedShard: number | undefined, pinnedCount: number, platform: string, githubActions: string | undefined }} DelegationRoute */

/** @param {DelegationRoute} route @returns {boolean} */
export function isDelegatedShard(route) {
  return route.suite === DELEGATED_SUITE &&
    route.platform === "linux" &&
    route.githubActions === "true" &&
    route.count === route.pinnedCount &&
    route.shard === route.pinnedShard;
}

/** @typedef {{ delegated: boolean, bin: string, args: string[], path: string, runMarker: string, brokerScope: string }} SuiteInvocationInput */

/** @param {SuiteInvocationInput} input @returns {{ bin: string, args: string[] }} */
export function suiteInvocation(input) {
  if (!input.delegated) return { bin: input.bin, args: ["--silent", ...input.args] };
  return {
    bin: "sudo",
    args: [
      "systemd-run", "--scope", "-p", "Delegate=yes", "--quiet", "--", "env",
      `PATH=${input.path}`,
      `COTAL_RUN=${input.runMarker}`,
      `SMOKE_BROKER_SCOPE=${input.brokerScope}`,
      "DO_NOT_TRACK=1",
      "COTAL_SEAT_CGROUP_DELEGATED=1",
      input.bin, "--silent", ...input.args,
    ],
  };
}

/** Keep only the explicit cgroup delegation declaration across a suite's COTAL_ scrub. */
/** @param {Record<string, string | undefined>} environment @returns {Record<string, string>} */
export function preserveSeatCgroupDelegation(environment) {
  return environment.COTAL_SEAT_CGROUP_DELEGATED === "1"
    ? { COTAL_SEAT_CGROUP_DELEGATED: "1" }
    : {};
}
