// Example layout for nemo-move tests. Every name is made up. A real
// deployment supplies its own file with --inventory and no default applies.
rows: [
	{path: ".example-agent", kind: "copy", dest: "/opt/data", skip: ["gateway.pid", "gateway.lock", "processes.json"]},
	{path: "example", kind: "copy", dest: "/opt/data/example", discover: [{dir: ".", known: ["reports", "archive"]}]},
	{path: "example/reports", kind: "copy", dest: "/opt/data/example/reports"},
	{path: "example/archive", kind: "copy", dest: "/opt/data/example/archive"},
	{path: "Library/Application Support/Example/Cache", kind: "rebuild", dest: "/opt/data/cache"},
	{path: "Library/Application Support/Example/old", kind: "retire", dest: "none"},
]

google: {
	source:   "/var/lib/example-google/accounts"
	dest:     "/opt/data/.local/state/example-google"
	accounts: ["workspace", "personal"]
}

journals: {
	dir:   "/var/lib/example-journals"
	names: ["pages.jsonl", "events.jsonl"]
}

processes: ["example-agent --serve", "example-tunnel run"]
