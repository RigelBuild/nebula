id: "akiflow"
release: {
	base: "0.1.0"
	artifacts: {
		"akiflow": {
			kind:     "binary"
			package:  "local:akiflow"
			binaries: ["akiflow"]
			main:     "./cmd/akiflow"
		}
	}
}
