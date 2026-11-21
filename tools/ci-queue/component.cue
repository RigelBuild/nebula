id: "ci-queue"
release: {
	base: "0.1.0"
	artifacts: {
		"ci-queue": {
			kind:     "binary"
			package:  "local:ci-queue"
			binaries: ["ci-queue"]
		}
	}
}
