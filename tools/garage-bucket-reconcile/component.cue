id: "garage-bucket-reconcile"
release: {
	base: "0.1.0"
	artifacts: {
		"garage-bucket-reconcile": {
			kind:     "binary"
			package:  "local:garage-bucket-reconcile"
			binaries: ["garage-bucket-reconcile"]
			main:     "./cmd/garage-bucket-reconcile"
		}
		"garage-admin-token-mint": {
			kind:     "binary"
			package:  "local:garage-admin-token-mint"
			binaries: ["garage-admin-token-mint"]
			main:     "./cmd/garage-admin-token-mint"
		}
	}
}
