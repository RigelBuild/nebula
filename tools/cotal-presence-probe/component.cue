id: "cotal-presence-probe"
release: {
	base: "0.1.0"
	artifacts: {
		"cotal-presence-probe": {
			kind:     "nix"
			attr:     "cotal-presence-probe"
			binaries: ["cotal-presence-probe"]
			runtimeDeps: [
				"libc.so.6",
				"ld-linux-x86-64.so.2",
				"libpthread.so.0",
				"libdl.so.2",
				"libm.so.6",
			]
		}
	}
}
