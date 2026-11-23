package main

// #Config is the caller's whole profile file. Every deployment value is a
// required field here: nebula supplies no default for any of them.
#Config: {
	// owners lists the GitHub owners rjc accepts repositories from.
	owners: [_, ...string]
	// coAuthor is the fallback trailer added to every commit and PR body.
	coAuthor: string & !=""
	// coAuthorByOwner optionally overrides coAuthor for a GitHub owner.
	coAuthorByOwner?: {
		[Owner=string]: string & !=""
	}
	// reviewer is the login requested as reviewer on tracking-fork PRs.
	reviewer: string & !=""
	// agentEmail is the jj user.email required in agent workspaces.
	agentEmail: string & =~"@"
	// mergeBotEmail is the jj user.email required in the merge-bot workspace.
	mergeBotEmail: string & =~"@"
	// mergeBotRepoDir is the merge-bot clone root, relative to $HOME.
	mergeBotRepoDir: string & !=""
	// aliases maps a repository name to the name rjc uses for it.
	aliases: [string]: string
	// gateRunnerDir is the directory of the gate-runner tool, relative to the repository root.
	gateRunnerDir: string & !=""
	// rootChecksDir is the directory of the root-checks tool, relative to the repository root.
	rootChecksDir: string & !=""
	// devenvNixpkgs pins the nixpkgs revision used to start a devenv shell.
	devenvNixpkgs: string & !=""
	// default supplies repository policy when repos has no matching entry.
	default?: #Repo
	repos: [Name=string]: #Repo & {name: Name}
}

#Repo: {
	name?: string
	pr:      *true | bool
	posture: *"none" | "tracking" | "owned"
	trunk:   *"main" | string
	if posture == "tracking" {
		mirror: string & !=""
		trunk:  "rigel-release"
	}
	// issuePrefix is the tracker key prefix for this repo, such as "EX" for
	// keys like EX-123. It has no default: every gate that reads issue keys
	// needs it.
	issuePrefix: string & =~"^[A-Za-z][A-Za-z0-9]*$"
	// specDirs lists directories, each ending in "/", that hold living specs.
	// At least one is required.
	specDirs: [_, ...string] & [...=~"/$"]
	ciGates:       *([]) | [...string]
	hooks:         *true | bool
	noHooksReason?: string & !=""
	if !hooks {
		noHooksReason!: string
	}
	remoteHooks:   *false | bool
	localFastTier: *false | bool
	hookReports:   *false | bool
	designLabel:   *false | bool
	baseFilesets:  *([]) | [...string]
	// skipChecks lists the root-checks checks a p0: skip still runs in the head tree.
	skipChecks: *([]) | [...string]
}
