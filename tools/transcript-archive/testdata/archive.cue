package archive

// Synthetic archive config for tests. It mirrors the shape of a real host
// config with placeholder names only, so nothing here names a real machine.

let sharedLayout = {
	sources: [
		{name: "sessions", path: ".example/agent/sessions"},
		{name: "blobs", path: ".example/agent/blobs"},
		{name: "custom-session-files", path: ".example/agent/custom-session-files"},
		{name: "logs", path: ".example/logs"},
		{name: "agent-db", path: ".example/agent/agent.db"},
		{name: "agent-db-wal", path: ".example/agent/agent.db-wal"},
		{name: "agent-db-shm", path: ".example/agent/agent.db-shm"},
	]
	watch: [".example"]
	exclude: [
		{path: ".example/cache"},
		{path: ".example/install-id"},
		{path: ".example/agent/config.yml"},
	]
	finalOnly: [
		".example/agent/agent.db",
		".example/agent/agent.db-wal",
		".example/agent/agent.db-shm",
	]
}

hosts: "example-host": {
	state: "unmeasured"
	sources: sharedLayout.sources
	watch: sharedLayout.watch
	exclude: sharedLayout.exclude
	finalOnly: sharedLayout.finalOnly
}

hosts: "example-empty": {
	state: "empty"
	sources: []
	watch: []
	exclude: []
	finalOnly: []
}
