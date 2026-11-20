id: "woodpecker"
images: {
	"woodpecker-agent-image": {
		repository: "ghcr.io/rigelbuild/woodpecker-agent"
		dockerfile: "docker/Dockerfile.agent.multiarch"
		platforms: ["linux/amd64", "linux/arm64"]
		// CD replaces {source} with the tested commit and {version} with
		// .github/rigel-version. The Makefile strips the leading v, so the
		// agent reports the bare version. Values match the fork publish workflow.
		buildArgs: {
			CI_COMMIT_SHA:    "{source}"
			CI_COMMIT_TAG:    "v{version}"
			CI_COMMIT_BRANCH: "rigel-release"
		}
	}
	"woodpecker-server-image": {
		repository: "ghcr.io/rigelbuild/woodpecker-server"
		dockerfile: "docker/Dockerfile.server.nebula"
		platforms: ["linux/amd64", "linux/arm64"]
		// Same build args as the agent. The Dockerfile builds the UI and the
		// server from source, so it needs no prebuilt binary.
		buildArgs: {
			CI_COMMIT_SHA:    "{source}"
			CI_COMMIT_TAG:    "v{version}"
			CI_COMMIT_BRANCH: "rigel-release"
		}
	}
}
release: {
	base: "3.17.0-rigel.27"
	artifacts: {
		"woodpecker-agent": {
			kind:     "nix"
			attr:     "woodpecker-agent-static"
			binaries: ["woodpecker-agent"]
		}
	}
}
