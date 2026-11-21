import { describe, expect, test } from "bun:test";
import { loadConfig } from "./config.ts";

const REQUIRED = {
	RUNTIME_DIRECTORY: "/run/example",
	WAVE_SEATS_FILE: "/etc/example/wave-seats.json",
};

describe("loadConfig", () => {
	test("applies the optional defaults when only the required values are set", () => {
		expect(loadConfig(REQUIRED)).toEqual({
			port: 9249,
			space: "main",
			server: "nats://127.0.0.1:4222",
			seatsFile: "/etc/example/wave-seats.json",
			runtimeDir: "/run/example",
		});
	});

	test("reads each optional override", () => {
		expect(
			loadConfig({
				...REQUIRED,
				COTAL_PRESENCE_PROBE_PORT: "9300",
				COTAL_SPACE: "dev",
				COTAL_SERVER: "nats://nats.example.com:4222",
			}),
		).toEqual({
			port: 9300,
			space: "dev",
			server: "nats://nats.example.com:4222",
			seatsFile: "/etc/example/wave-seats.json",
			runtimeDir: "/run/example",
		});
	});

	test("rejects a missing WAVE_SEATS_FILE", () => {
		expect(() => loadConfig({ RUNTIME_DIRECTORY: "/run/example" })).toThrow(
			"WAVE_SEATS_FILE is required",
		);
	});

	test("rejects an empty WAVE_SEATS_FILE", () => {
		expect(() => loadConfig({ ...REQUIRED, WAVE_SEATS_FILE: "" })).toThrow(
			"WAVE_SEATS_FILE is required",
		);
	});

	test("rejects a missing RUNTIME_DIRECTORY", () => {
		expect(() =>
			loadConfig({ WAVE_SEATS_FILE: "/etc/example/wave-seats.json" }),
		).toThrow("RUNTIME_DIRECTORY is required");
	});
});
