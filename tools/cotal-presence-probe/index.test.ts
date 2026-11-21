import { describe, expect, test } from "bun:test";
import {
	AGE_SERIES_CAP,
	connectionTracker,
	HISTORY_REFUSAL_PREFIX,
	loadSeatNames,
	type PresenceRow,
	renderMetrics,
	seatLabel,
} from "./index.ts";

const NOW = 1_800_000_000_000;
const SEATS = new Set(["seat-a", "seat-b", "seat-c"]);
const REFUSAL = `${HISTORY_REFUSAL_PREFIX}; retries stopped`;

function wedged(name: string, ageSeconds: number): PresenceRow {
	return {
		card: { name, kind: "agent" },
		status: "waiting",
		activity: REFUSAL,
		statusSince: NOW - ageSeconds * 1000,
	};
}

function snap(roster: PresenceRow[], current = true) {
	return { current, roster };
}

function ageLines(text: string): string[] {
	return text
		.split("\n")
		.filter((l) => l.startsWith("cotal_seat_history_refused_age_seconds{"));
}

describe("renderMetrics: which seats emit an age series", () => {
	test("wedged agent seat emits one age series", () => {
		const out = renderMetrics(snap([wedged("seat-a", 700)]), NOW, SEATS);
		expect(ageLines(out)).toEqual([
			'cotal_seat_history_refused_age_seconds{seat="seat-a"} 700',
		]);
		expect(out).toContain("cotal_seats_history_refused 1\n");
	});

	test("offline seat emits no age series", () => {
		const row: PresenceRow = {
			card: { name: "seat-a", kind: "agent" },
			status: "offline",
			activity: REFUSAL,
		};
		const out = renderMetrics(snap([row]), NOW, SEATS);
		expect(ageLines(out)).toEqual([]);
		expect(out).toContain("cotal_seats_history_refused 0\n");
	});

	test("other-activity waiting seat emits no age series", () => {
		const row: PresenceRow = {
			card: { name: "seat-a", kind: "agent" },
			status: "waiting",
			activity: "waiting for review",
			statusSince: NOW - 900_000,
		};
		expect(ageLines(renderMetrics(snap([row]), NOW, SEATS))).toEqual([]);
	});

	test("endpoint-kind seat with the refusal emits no age series", () => {
		const row: PresenceRow = {
			card: { name: "seat-a", kind: "endpoint" },
			status: "waiting",
			activity: REFUSAL,
			statusSince: NOW - 900_000,
		};
		expect(ageLines(renderMetrics(snap([row]), NOW, SEATS))).toEqual([]);
	});
});

describe("renderMetrics: wedge guards", () => {
	test("a working seat with the refusal text is not wedged", () => {
		const row: PresenceRow = {
			card: { name: "seat-a", kind: "agent" },
			status: "working",
			activity: REFUSAL,
			statusSince: NOW - 700_000,
		};
		const out = renderMetrics(snap([row]), NOW, SEATS);
		expect(ageLines(out)).toEqual([]);
		expect(out).toContain("cotal_seats_history_refused 0\n");
	});

	test("a waiting seat with no statusSince is not wedged", () => {
		const row: PresenceRow = {
			card: { name: "seat-a", kind: "agent" },
			status: "waiting",
			activity: REFUSAL,
		};
		const out = renderMetrics(snap([row]), NOW, SEATS);
		expect(ageLines(out)).toEqual([]);
		expect(out).toContain("cotal_seats_history_refused 0\n");
	});

	test("a waiting seat whose activity holds the prefix mid-string is not wedged", () => {
		const row: PresenceRow = {
			card: { name: "seat-a", kind: "agent" },
			status: "waiting",
			activity: `halted; ${REFUSAL}`,
			statusSince: NOW - 700_000,
		};
		const out = renderMetrics(snap([row]), NOW, SEATS);
		expect(ageLines(out)).toEqual([]);
		expect(out).toContain("cotal_seats_history_refused 0\n");
	});
});

describe("renderMetrics: age series merge and cap", () => {
	test("two seats with the same seat label merge to one series keeping the max age", () => {
		const out = renderMetrics(
			snap([wedged("seat-a", 100), wedged("seat-a", 900)]),
			NOW,
			SEATS,
		);
		expect(ageLines(out)).toEqual([
			'cotal_seat_history_refused_age_seconds{seat="seat-a"} 900',
		]);
	});

	test("unknown names with the same other label merge to one series keeping the max age", () => {
		const out = renderMetrics(
			snap([wedged("zz-a", 100), wedged("zz-b", 400)]),
			NOW,
			SEATS,
		);
		expect(ageLines(out)).toEqual([
			'cotal_seat_history_refused_age_seconds{seat="other"} 400',
		]);
	});

	test("21 distinct wedged seats emit 20 age series and a count of 21", () => {
		const many = new Set(Array.from({ length: 21 }, (_, i) => `seat-${i}`));
		const roster = [...many].map((name, i) => wedged(name, 100 + i));
		const out = renderMetrics(snap(roster), NOW, many);
		expect(ageLines(out)).toHaveLength(AGE_SERIES_CAP);
		expect(out).toContain("cotal_seats_history_refused 21\n");
	});

	test("the 20 kept series are the oldest seats", () => {
		const many = new Set(Array.from({ length: 21 }, (_, i) => `seat-${i}`));
		const roster = [...many].map((name, i) => wedged(name, i));
		const out = renderMetrics(snap(roster), NOW, many);
		expect(out).not.toContain('seat="seat-0"');
		expect(out).toContain('seat="seat-20"');
	});
});

describe("renderMetrics: seat counts and probe_up", () => {
	test("cotal_seats always has all four statuses, even at zero", () => {
		const out = renderMetrics(snap([]), NOW, SEATS);
		for (const s of ["idle", "waiting", "working", "offline"]) {
			expect(out).toContain(`cotal_seats{status="${s}"} 0\n`);
		}
	});

	test("cotal_seats counts live rows per status", () => {
		const roster: PresenceRow[] = [
			{ card: { name: "seat-a" }, status: "working" },
			{ card: { name: "seat-b" }, status: "working" },
			{ card: { name: "seat-c" }, status: "offline" },
		];
		const out = renderMetrics(snap(roster), NOW, SEATS);
		expect(out).toContain('cotal_seats{status="working"} 2\n');
		expect(out).toContain('cotal_seats{status="offline"} 1\n');
		expect(out).toContain('cotal_seats{status="idle"} 0\n');
	});

	test("non-current view emits only probe_up 0", () => {
		const out = renderMetrics(snap([wedged("seat-a", 900)], false), NOW, SEATS);
		expect(out).toContain("cotal_presence_probe_up 0\n");
		expect(out).not.toContain("cotal_seats");
		expect(out).not.toContain("cotal_seat_history_refused_age_seconds{");
	});

	test("current view emits probe_up 1", () => {
		expect(renderMetrics(snap([]), NOW, SEATS)).toContain(
			"cotal_presence_probe_up 1\n",
		);
	});
});

describe("renderMetrics: seat labels", () => {
	test("allowlisted name seat-a keeps its label", () => {
		const out = renderMetrics(snap([wedged("seat-a", 600)]), NOW, SEATS);
		expect(out).toContain('seat="seat-a"');
	});

	test("name absent from the allowlist maps to seat=other", () => {
		const out = renderMetrics(snap([wedged("zz-unknown", 600)]), NOW, SEATS);
		expect(out).toContain('seat="other"');
		expect(out).not.toContain("zz-unknown");
	});

	test("the seat label escapes quotes and backslashes", () => {
		const seats = new Set(['a"b\\c']);
		const out = renderMetrics(snap([wedged('a"b\\c', 600)]), NOW, seats);
		expect(out).toContain('seat="a\\"b\\\\c"');
	});
});

describe("seatLabel", () => {
	test("returns the name when it is in the allowlist", () => {
		expect(seatLabel("seat-b", SEATS)).toBe("seat-b");
	});
	test("returns other for names outside the allowlist", () => {
		expect(seatLabel("zz-unknown", SEATS)).toBe("other");
	});
});

describe("loadSeatNames", () => {
	const file = (seats: unknown[]) =>
		JSON.stringify({ version: 2, revision: 1, seats });

	test("excludes retired seats", () => {
		const names = loadSeatNames(
			file([
				{ name: "seat-a", state: "active" },
				{ name: "seat-retired", state: "retired" },
			]),
		);
		expect([...names]).toEqual(["seat-a"]);
	});

	test("keeps paused seats", () => {
		const names = loadSeatNames(
			file([{ name: "seat-paused", state: "paused" }]),
		);
		expect(names.has("seat-paused")).toBe(true);
	});

	test("rejects a file that is not JSON", () => {
		expect(() => loadSeatNames("{not json")).toThrow();
	});

	test("rejects a file with no seats array", () => {
		expect(() => loadSeatNames(JSON.stringify({ version: 2 }))).toThrow(
			"missing seats array",
		);
	});

	test("rejects a seat entry with no name", () => {
		expect(() => loadSeatNames(file([{ state: "active" }]))).toThrow(
			"seat name is not a non-empty string",
		);
	});

	test("rejects a seat entry with an unknown state", () => {
		expect(() =>
			loadSeatNames(file([{ name: "seat-a", state: "limbo" }])),
		).toThrow("unknown state");
	});
});

describe("connectionTracker", () => {
	test("starts down, rises on connect, falls on disconnect", () => {
		const link = connectionTracker();
		expect(link.isConnected()).toBe(false);
		link.onEvent({ connected: true });
		expect(link.isConnected()).toBe(true);
		link.onEvent({ connected: false });
		expect(link.isConnected()).toBe(false);
	});
});
