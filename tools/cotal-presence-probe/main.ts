// Entry point for the cotal-presence-probe service. It opens a read-only
// Cotal observer on one space and serves Prometheus metrics on loopback.
// Every scrape calls presenceView() and getRoster() afresh. The presence-view
// event fires only on sweep and teardown, so a cached value would go stale.

import { readFileSync } from "node:fs";
import { CotalEndpoint } from "@cotal-ai/core";
import { loadConfig, type ProbeConfig } from "./config.ts";
import {
	connectionTracker,
	loadSeatNamesFile,
	renderMetrics,
} from "./index.ts";

function fail(message: string): never {
	console.error(`cotal-presence-probe: ${message}`);
	process.exit(1);
}

let config: ProbeConfig;
try {
	config = loadConfig(Bun.env);
} catch (err) {
	fail((err as Error).message);
}

// Missing or unparseable seat files exit non-zero, so the absence alert fires.
// The list is read once at start; restart the probe to refresh it.
let seats: Set<string>;
try {
	seats = loadSeatNamesFile(config.seatsFile);
} catch (err) {
	fail(`cannot load seats from ${config.seatsFile}: ${(err as Error).message}`);
}

// observer.creds must exist in the runtime directory before the probe starts.
// CotalEndpoint takes the creds file content, not a path.
const creds = readFileSync(`${config.runtimeDir}/observer.creds`, "utf8");

const endpoint = new CotalEndpoint({
	space: config.space,
	servers: config.server,
	card: { name: "cotal-presence-probe", kind: "endpoint" },
	creds,
	channels: [],
	consume: false,
	registerPresence: false,
	watchPresence: true,
	watchChannels: false,
});

// probe_up is derived from this tracker, which starts down. A `connection`
// event sets it; the scrape path reads it.
const link = connectionTracker();
endpoint.on("connection", (ev: { connected: boolean }) => {
	const before = link.isConnected();
	link.onEvent(ev);
	if (before !== link.isConnected()) {
		console.error(
			`cotal-presence-probe: connection ${link.isConnected() ? "up" : "down"}`,
		);
	}
});
endpoint.on("warning", (err: Error) => {
	console.error(`cotal-presence-probe: warning: ${err.message}`);
});
endpoint.on("error", (err: Error) => {
	console.error(`cotal-presence-probe: error: ${err.message}`);
});

try {
	await endpoint.start();
} catch (err) {
	fail(`start failed: ${(err as Error).message}`);
}
await endpoint.waitForPresenceSnapshot(5_000);

// Log each view change (state only) so a stale or unpopulated view is visible
// in the journal without a line per scrape.
let lastView = "";
function logViewChange(): void {
	const state = endpoint.presenceView().state;
	if (state === lastView) return;
	lastView = state;
	console.error(`cotal-presence-probe: presence view ${state}`);
}

const server = Bun.serve({
	hostname: "127.0.0.1",
	port: config.port,
	fetch(req) {
		const path = new URL(req.url).pathname;
		if (path === "/healthz") return new Response("ok\n");
		if (path !== "/metrics") {
			return new Response("not found\n", { status: 404 });
		}
		logViewChange();
		// probe_up is 1 only when the transport is up and the view is current.
		const current =
			link.isConnected() && endpoint.presenceView().state === "current";
		const body = renderMetrics(
			{ current, roster: endpoint.getRoster() },
			Date.now(),
			seats,
		);
		return new Response(body, {
			headers: { "content-type": "text/plain; version=0.0.4" },
		});
	},
});
console.error(`cotal-presence-probe: serving /metrics on ${server.url}`);

async function shutdown(): Promise<void> {
	server.stop();
	await endpoint.stop();
	process.exit(0);
}
process.on("SIGTERM", () => void shutdown());
process.on("SIGINT", () => void shutdown());
