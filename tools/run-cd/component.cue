id: "run-cd"
release: {
	base: "0.1.0"
	artifacts: {
		"run-cd": {
			kind:     "binary"
			package:  "local:run-cd"
			binaries: ["run-cd"]
		}
	}
}
