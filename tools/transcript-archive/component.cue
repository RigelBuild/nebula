id: "transcript-archive"
release: {
	base: "0.1.0"
	artifacts: {
		"transcript-archive": {
			kind:     "binary"
			package:  "local:transcript-archive"
			binaries: ["transcript-archive"]
			main:     "./cmd/transcript-archive"
		}
	}
}
