id: "alloy-harness"
release: {
	base: "0.1.0"
	artifacts: {
		"alloy-event-feed": {
			kind:     "binary"
			package:  "local:alloy-event-feed"
			binaries: ["alloy-event-feed"]
			main:     "./cmd/alloy-event-feed"
		}
		"alloy-scrape-limits": {
			kind:     "binary"
			package:  "local:alloy-scrape-limits"
			binaries: ["alloy-scrape-limits"]
			main:     "./cmd/alloy-scrape-limits"
		}
	}
}
