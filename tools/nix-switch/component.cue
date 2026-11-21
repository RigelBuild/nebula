id: "nix-switch"
release: {
	base: "0.4.0"
	artifacts: {
		"nix-switch": {
			kind:     "binary"
			package:  "local:nix-switch"
			binaries: ["nix-switch"]
		}
	}
}
