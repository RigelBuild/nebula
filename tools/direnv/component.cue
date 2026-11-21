id: "direnv"
release: {
	base: "2.38.2"
	artifacts: {
		"direnv": {
			kind:     "nix"
			attr:     "direnv"
			binaries: ["direnv"]
		}
	}
}
