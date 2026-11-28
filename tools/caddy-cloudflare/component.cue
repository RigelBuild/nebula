id: "caddy-cloudflare"
release: {
	base: "2.11.9"
	artifacts: {
		"caddy-cloudflare": {
			kind:     "nix"
			attr:     "caddy-cloudflare"
			binaries: ["caddy"]
		}
	}
}
