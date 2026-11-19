id: "autoscaler"
images: {
	autoscaler: {
		repository: "ghcr.io/rigelbuild/autoscaler"
		dockerfile: "docker/Dockerfile"
		platforms: ["linux/amd64", "linux/arm64"]
		// The build context has no .git, so the Makefile takes the commit and
		// tag from here. CD replaces {source} with the tested commit and {tag}
		// with this Release's tag. The Dockerfile declares both as ARG.
		buildArgs: {
			CI_COMMIT_SHA: "{source}"
			CI_COMMIT_TAG: "{tag}"
		}
	}
}
