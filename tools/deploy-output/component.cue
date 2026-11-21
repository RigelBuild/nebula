id: "deploy-output"
release: {
	base: "0.1.0"
	artifacts: {
		"deploy-output": {
			kind:     "binary"
			package:  "local:deploy-output"
			binaries: ["deploy-output"]
		}
	}
}
