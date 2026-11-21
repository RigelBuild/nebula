#Config: {
	// Repository checkout. A relative path resolves against $HOME.
	repo: string & !=""
	// Directory inside repo that holds the darwin flake. It is relative and has
	// no ".." segment, so it cannot leave the checkout.
	flake: string & =~"^[^/]" & !~"(^|/)\\.\\.(/|$)"
	// Names of darwinConfigurations that --host may select. At least one.
	hosts: [...(string & !="")] & [_, ...]
}
