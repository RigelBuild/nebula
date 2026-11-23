// release-pending is the release gate's config. checkReleaseGate and
// loadGateConfig in tools/cd/release_gate_test.go read it.
//
// pending lists components that ship no release, images or published form yet.
// The gate allows each listed project and fails any other component that ships
// nothing. A listed project that starts to ship fails the gate, so this list
// only shrinks. Each entry names its tracking issue.
//
// sidePaths lists components whose release a named CD side path publishes,
// outside the plan. The gate treats each one as published.

pending: [=~"^(tools|forks)/[a-z0-9-]+$"]: {
	issue: =~"^RIG-[0-9]+$"
}

pending: {
	"forks/cla-github-action": issue:                  "RIG-12303"
	"forks/hermes-agent": issue:                       "RIG-12299"
	"forks/terraform-provider-googleworkspace": issue: "RIG-12303"
	"forks/virtual": issue:                            "RIG-12303"
	"forks/wails": issue:                              "RIG-12303"
}

sidePaths: [=~"^(tools|forks)/[a-z0-9-]+$"]: {
	kind: string
	ref:  string & != ""
}

sidePaths: {
	// The cd.yaml runtime job uploads cotal-runtime-<platform>.tar.gz.
	"forks/cotal": {
		kind: "runtime-tarball"
		ref:  "cd.yaml runtime job"
	}
}
