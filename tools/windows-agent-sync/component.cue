id: "windows-agent-sync"
release: {
	base: "0.4.0"
	artifacts: {
		"windows-agent-sync": {
			kind:     "binary"
			package:  "local:windows-agent-sync"
			binaries: ["windows-agent-sync"]
			systems: ["windows-amd64"]
		}
	}
}
