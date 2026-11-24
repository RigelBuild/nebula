id: "sops-install-secrets"
release: {
	base: "0.0.1"
	artifacts: {
		"sops-install-secrets": {
			kind:     "nix"
			attr:     "sops-install-secrets"
			binaries: ["sops-install-secrets"]
			systems: ["x86_64-linux", "aarch64-linux"]
		}
	}
}
