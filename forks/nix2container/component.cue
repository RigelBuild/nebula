id: "nix2container"
release: {
	base: "1.1.0-rigel.2"
	artifacts: {
		"nix2container": {
			kind:     "nix"
			attr:     "nix2container-static"
			binaries: ["nix2container"]
		}
	}
}
