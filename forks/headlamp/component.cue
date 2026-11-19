id: "headlamp"
images: {
	headlamp: {
		repository: "ghcr.io/rigelbuild/headlamp"
		dockerfile: "Dockerfile"
		platforms: ["linux/amd64", "linux/arm64"]
		// The build context has no .git, so the frontend build cannot read the
		// git version. CD replaces {source} with the tested commit, and the
		// Dockerfile declares the value as ARG HEADLAMP_GIT_VERSION. The
		// frontend build writes it into the bundle, where the smoke test reads it.
		buildArgs: {
			HEADLAMP_GIT_VERSION: "{source}"
		}
	}
}
