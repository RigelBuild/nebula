id: "trunk-mq-mcp"
release: {
	base: "0.1.0"
	artifacts: {
		"trunk-mq-mcp": {
			kind:     "nix"
			attr:     "trunk-mq-mcp"
			binaries: ["trunk-mq-mcp"]
			runtimeDeps: [
				"libc.so.6",
				"ld-linux-x86-64.so.2",
				"libpthread.so.0",
				"libdl.so.2",
				"libm.so.6",
				"ld-linux-aarch64.so.1",
			]
		}
	}
}
