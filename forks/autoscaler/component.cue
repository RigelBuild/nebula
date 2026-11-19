id: "autoscaler"
images: {
	autoscaler: {
		repository: "ghcr.io/rigelbuild/autoscaler"
		dockerfile: "docker/Dockerfile"
		platforms: ["linux/amd64", "linux/arm64"]
		// The build context has no .git, so the Makefile takes the commit and
		// version from here. CD replaces {source} with the tested commit and
		// {version} with the fork's .github/rigel-version. The Dockerfile
		// declares both as ARG. The Makefile strips a leading v, so the bare
		// version is passed.
		buildArgs: {
			CI_COMMIT_SHA: "{source}"
			CI_COMMIT_TAG: "{version}"
		}
	}
}
