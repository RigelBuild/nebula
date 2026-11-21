// Example profile for rjc tests. Every value is generic: no real owner, host, or identity.
package main

owners: ["example"]
coAuthor: "Example Author <author@example.com>"
reviewer: "example-reviewer"
agentEmail: "agent@example.com"
mergeBotEmail: "merge@example.com"
mergeBotRepoDir: "merge/repos"
aliases: {
	"example/factory-alias": "example/factory"
}
gateRunnerDir: "tools/gate-runner"
rootChecksDir: "tools/root-checks"
devenvNixpkgs: "github:example/nixpkgs/0000000000000000000000000000000000000000"

repos: {
	"example/app": {
		issuePrefix: "EX"
		specDirs: ["docs/specs/"]
		ciGates: ["pr-title-issue-ref", "branch-name", "spec-impact", "metadata-bounds"]
		remoteHooks:   true
		hookReports:   false
		localFastTier: true
		designLabel:   true
		skipChecks: ["biome", "markdownlint", "project-registry"]
	}
	"example/docs": {
		issuePrefix: "EX"
		specDirs: ["docs/"]
		ciGates: ["metadata-bounds"]
	}
	"example/factory": {
		issuePrefix: "EX"
		specDirs: ["docs/specs/"]
		hooks:         false
		noHooksReason: "example has no pre-push hooks (EX-1)"
		baseFilesets:  ["migrations", "proto"]
	}
	"example/mirror-test": {
		issuePrefix: "EX"
		specDirs: ["docs/"]
		pr: false
	}
	"example/owned-fork": {
		issuePrefix: "EX"
		specDirs: ["docs/"]
		posture: "owned"
	}
	"example/tracked-fork": {
		issuePrefix: "EX"
		specDirs: ["docs/"]
		posture: "tracking"
		mirror:  "main"
	}
	"example/tracked-master": {
		issuePrefix: "EX"
		specDirs: ["docs/"]
		posture: "tracking"
		mirror:  "master"
	}
	"example/jj-vine": {
		issuePrefix: "EX"
		specDirs: ["docs/"]
		hooks:         false
		noHooksReason: "jj-vine has no pre-push hook config (EX-1)"
	}
	"example/tool": {
		issuePrefix: "EX"
		specDirs: ["docs/"]
		hooks:         false
		noHooksReason: "example has no pre-push hook config (EX-1)"
	}
}
