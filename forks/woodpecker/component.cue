id: "woodpecker-agent"
release: {
	base: "3.17.0-rigel.27"
	artifacts: {
		"woodpecker-agent": {
			kind:     "nix"
			attr:     "woodpecker-agent-static"
			binaries: ["woodpecker-agent"]
		}
	}
}
