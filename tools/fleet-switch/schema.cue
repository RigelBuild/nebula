#Host: {
	name: string & !=""
	target: string & !=""
	platform: "nixos" | "darwin" | "wsl" | "windows"
	// Windows hosts run over ssh, not nix, so they have no flake.
	if platform != "windows" {
		flake: string & !=""
	}
	rebootable: bool
	if platform == "windows" {
		rebootable: false
		// Checkout and setup script are relative to the Windows user profile.
		// No quote, dollar sign or backtick: each value is spliced into a PowerShell script.
		windowsCheckout: string & =~#"^[^'"$`]+$"#
		windowsSetup:    string & =~#"^[^'"$`]+$"#
	}
	user: *"root" | string
	port: (*22 | int) & >=1 & <=65535
	identityFile?: string
	gamingFlag?: string
	gamingProbeHost?: string & !=""
	// Omitted from --all; these hosts run only when named.
	explicitOnly?: true
	// The only host type that may deploy a --flake PATH local checkout.
	localCheckout?: true
}
