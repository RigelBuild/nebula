id: "oh-my-pi"
images: {
	omp: {
		repository: "ghcr.io/rigelbuild/omp"
		dockerfile: "Dockerfile.omp"
		platforms: ["linux/amd64"]
	}
}
release: {
	base: "18.22.2-rigel.2"
	artifacts: {
		"omp-bin": {
			kind:     "nix"
			attr:     "omp-release"
			binaries: ["omp"]
			runtimeDeps: [
				"libc.so.6",
				"ld-linux-x86-64.so.2",
				"ld-linux-aarch64.so.1",
				"libpthread.so.0",
				"libdl.so.2",
				"libm.so.6",
				"libgcc_s.so.1",
			]
		}
	}
}
