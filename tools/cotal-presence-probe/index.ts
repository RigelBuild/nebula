// Renders Cotal mesh presence for one space as Prometheus gauges. The probe is
// a read-only observer: it writes no presence and consumes no messages. A seat
// whose provider refused its session history holds in `waiting` with a fixed
// activity line until restarted; `cotal_seat_history_refused_age_seconds` dates
// that hold so one alert can page a human within minutes.

import { readFileSync } from "node:fs";

export const HISTORY_REFUSAL_PREFIX = "provider refused this session's history";
export const AGE_SERIES_CAP = 20;

export type PresenceRow = {
	card: { name: string; kind?: "agent" | "endpoint" };
	status: "idle" | "waiting" | "working" | "offline";
	activity?: string;
	statusSince?: number; // epoch ms
};

export type PresenceSnapshot = {
	current: boolean;
	roster: readonly PresenceRow[];
};

export type SeatName = string;

const STATUSES = ["idle", "waiting", "working", "offline"] as const;

// Seat names come from wave-seats.json, so the label set is fixed by that file.
// Any other card name maps to "other" so a new mesh name cannot add series.
export function seatLabel(
	cardName: string,
	seats: ReadonlySet<SeatName>,
): SeatName | "other" {
	return seats.has(cardName) ? cardName : "other";
}

function isWedged(row: PresenceRow): boolean {
	return (
		row.card.kind === "agent" &&
		row.status === "waiting" &&
		(row.activity ?? "").startsWith(HISTORY_REFUSAL_PREFIX) &&
		row.statusSince !== undefined
	);
}

function escapeLabel(value: string): string {
	return value
		.replace(/\\/g, "\\\\")
		.replace(/\n/g, "\\n")
		.replace(/"/g, '\\"');
}

export function renderMetrics(
	snap: PresenceSnapshot,
	nowMs: number,
	seats: ReadonlySet<SeatName>,
): string {
	const up = snap.current ? 1 : 0;
	let out =
		"# HELP cotal_presence_probe_up 1 when the observer is connected and its presence view is current.\n" +
		"# TYPE cotal_presence_probe_up gauge\n" +
		`cotal_presence_probe_up ${up}\n`;
	if (!snap.current) return out;

	const counts = new Map<PresenceRow["status"], number>(
		STATUSES.map((s) => [s, 0]),
	);
	for (const row of snap.roster) {
		counts.set(row.status, (counts.get(row.status) ?? 0) + 1);
	}
	out +=
		"# HELP cotal_seats Mesh seats in the main space by presence status.\n" +
		"# TYPE cotal_seats gauge\n";
	for (const s of STATUSES) {
		out += `cotal_seats{status="${s}"} ${counts.get(s) ?? 0}\n`;
	}

	const wedged = snap.roster.filter(isWedged);
	out +=
		"# HELP cotal_seats_history_refused Agent seats held after their provider refused session history.\n" +
		"# TYPE cotal_seats_history_refused gauge\n" +
		`cotal_seats_history_refused ${wedged.length}\n`;

	// Two roster rows can map to one seat label (an allowlisted name, or
	// several unknown names mapped to "other"). Keep the oldest hold per label
	// so the label set stays unique.
	const ageBySeat = new Map<SeatName | "other", number>();
	for (const row of wedged) {
		const seat = seatLabel(row.card.name, seats);
		const ageSeconds = Math.max(0, (nowMs - (row.statusSince ?? nowMs)) / 1000);
		const prev = ageBySeat.get(seat);
		if (prev === undefined || ageSeconds > prev)
			ageBySeat.set(seat, ageSeconds);
	}
	const oldest = [...ageBySeat.entries()]
		.sort((a, b) => b[1] - a[1])
		.slice(0, AGE_SERIES_CAP);
	out +=
		"# HELP cotal_seat_history_refused_age_seconds Seconds a seat has been held after its provider refused session history.\n" +
		"# TYPE cotal_seat_history_refused_age_seconds gauge\n";
	for (const [seat, age] of oldest) {
		out += `cotal_seat_history_refused_age_seconds{seat="${escapeLabel(seat)}"} ${age}\n`;
	}
	return out;
}

// wave-seats.json is the fleet's seat registry. Retired entries are dropped.
// Active and paused entries both keep their label: a paused seat still has a
// name that can wedge.
export function loadSeatNames(json: string): Set<SeatName> {
	const parsed: unknown = JSON.parse(json);
	if (typeof parsed !== "object" || parsed === null || !("seats" in parsed)) {
		throw new Error("wave-seats.json: missing seats array");
	}
	const seats = parsed.seats;
	if (!Array.isArray(seats)) {
		throw new Error("wave-seats.json: seats is not an array");
	}
	const names = new Set<SeatName>();
	for (const entry of seats) {
		if (typeof entry !== "object" || entry === null) {
			throw new Error("wave-seats.json: seat entry is not an object");
		}
		const { name, state } = entry as { name?: unknown; state?: unknown };
		if (typeof name !== "string" || name === "") {
			throw new Error("wave-seats.json: seat name is not a non-empty string");
		}
		if (state === "retired") continue;
		if (state !== "active" && state !== "paused") {
			throw new Error(`wave-seats.json: seat ${name} has unknown state`);
		}
		names.add(name);
	}
	return names;
}

export function loadSeatNamesFile(path: string): Set<SeatName> {
	return loadSeatNames(readFileSync(path, "utf8"));
}

export type ConnectionTracker = {
	onEvent(ev: { connected: boolean }): void;
	isConnected(): boolean;
};

// Tracks the observer's transport link from the endpoint's `connection` event.
// It starts down, so probe_up stays 0 until the first connect.
export function connectionTracker(): ConnectionTracker {
	let connected = false;
	return {
		onEvent(ev) {
			connected = ev.connected;
		},
		isConnected() {
			return connected;
		},
	};
}
