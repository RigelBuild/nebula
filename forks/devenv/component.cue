id: "devenv"
release: {
	base: "2.4.1-rigel.1"
	artifacts: {
		"devenv": {
			kind:     "nix"
			attr:     "devenv-static-bundle"
			// devenv-proxy is left out. Only `devenv up` with process.proxy.enable
			// spawns it (devenv/src/proxy.rs ensure_running); no shell, test or
			// switch path does, and devenv-run-tests never reaches it.
			binaries: ["devenv", "devenv-run-tests", "secretspec"]
			systems: ["x86_64-linux", "aarch64-linux", "aarch64-darwin"]
		}
	}
}
