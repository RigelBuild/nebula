// Reads the probe's environment. A value with no default is required: a
// missing one throws at start, so the probe never points at a path that does
// not exist on the host it runs on.

export type Env = Readonly<Record<string, string | undefined>>;

export type ProbeConfig = {
	port: number;
	space: string;
	server: string;
	seatsFile: string;
	runtimeDir: string;
};

function required(env: Env, name: string): string {
	const value = env[name];
	if (!value) throw new Error(`${name} is required`);
	return value;
}

export function loadConfig(env: Env): ProbeConfig {
	return {
		port: Number(env.COTAL_PRESENCE_PROBE_PORT ?? "9249"),
		space: env.COTAL_SPACE ?? "main",
		server: env.COTAL_SERVER ?? "nats://127.0.0.1:4222",
		seatsFile: required(env, "WAVE_SEATS_FILE"),
		runtimeDir: required(env, "RUNTIME_DIRECTORY"),
	};
}
