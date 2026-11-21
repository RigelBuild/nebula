#Config: close({
	// Repository checkout. A relative path resolves against $HOME.
	repo: string & !=""
	// Directory inside repo, relative to it, that holds the darwin flake.
	flake: string & =~"^[^/]"
	// Names of darwinConfigurations that --host may select. At least one.
	hosts: [...(string & !="")] & [_, ...]
})
