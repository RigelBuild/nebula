// Example inventory for tests. Real inventories are supplied with --hosts.
// The loader unifies every entry with #Host from schema.cue.
hosts: [
	{name: "darwin1", target: "darwin1.example.com", platform: "darwin", user: "example-admin", flake: "example/nix#darwin1", rebootable: false},
	{name: "nixos1", target: "nixos1.example.com", platform: "nixos", user: "example-admin", flake: "example/nix#nixos1", rebootable: true},
	{name: "nixos2", target: "nixos2.example.com", platform: "nixos", user: "example-admin", flake: "example/nix#nixos2", rebootable: true},
	{name: "nixos3", target: "nixos3.example.com", platform: "nixos", user: "example-admin", flake: "example/nix#nixos3", rebootable: true},
	{name: "nixos4", target: "nixos4.example.com", platform: "nixos", user: "example-admin", flake: "example/nix#nixos4", rebootable: true},
	{name: "nixos5", target: "nixos5.example.com", platform: "nixos", user: "example-admin", flake: "example/nix#nixos5", rebootable: true},
	{name: "wsl1", target: "wsl1.example.com", platform: "wsl", user: "exampleuser", port: 2222, identityFile: "~/.ssh/id_example", flake: "example/wsl#wsl1", rebootable: false, gamingFlag: "/var/lib/example-mode/gaming", localCheckout: true},
	{name: "win1", target: "wsl1.example.com", platform: "windows", user: "exampleuser", port: 22, gamingFlag: "/var/lib/example-mode/gaming", gamingProbeHost: "wsl1", explicitOnly: true, rebootable: false, windowsCheckout: "repos\\example", windowsSetup: "setup\\windows-setup.ps1"},
]
