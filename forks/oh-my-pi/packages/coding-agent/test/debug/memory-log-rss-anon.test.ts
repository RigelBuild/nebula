import { describe, expect, it } from "bun:test";
import { parseRssAnon } from "@oh-my-pi/pi-coding-agent/debug/memory-log";

const SAMPLE_STATUS = `Name:\tbun
Umask:\t0022
State:\tS (sleeping)
Tgid:\t12345
Pid:\t12345
VmPeak:\t 9000000 kB
VmSize:\t 8500000 kB
VmRSS:\t 2100000 kB
RssAnon:\t  1600000 kB
RssFile:\t   400000 kB
RssShmem:\t      100 kB
VmSwap:\t        0 kB
Threads:\t13
`;

describe("parseRssAnon", () => {
	it("reads the RssAnon line from a /proc/self/status sample into bytes", () => {
		expect(parseRssAnon(SAMPLE_STATUS)).toBe(1_600_000 * 1024);
	});

	it("does not confuse RssAnon with VmRSS or RssFile", () => {
		const onlyOthers = "VmRSS:\t 2100000 kB\nRssFile:\t   400000 kB\n";
		expect(parseRssAnon(onlyOthers)).toBeUndefined();
	});

	it("returns undefined when the RssAnon line is absent", () => {
		expect(parseRssAnon("Name:\tbun\nThreads:\t13\n")).toBeUndefined();
	});
});
