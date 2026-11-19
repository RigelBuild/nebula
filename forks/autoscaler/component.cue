id: "autoscaler"
images: {
	autoscaler: {
		repository: "ghcr.io/rigelbuild/autoscaler"
		dockerfile: "docker/Dockerfile"
		platforms: ["linux/amd64", "linux/arm64"]
	}
}
