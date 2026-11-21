package hookshell

// Select loads Nix and selects the shell for the checked-out tree.
const Select = `if [ -f /nix/var/nix/profiles/default/etc/profile.d/nix-daemon.sh ]; then . /nix/var/nix/profiles/default/etc/profile.d/nix-daemon.sh; fi
if [ -n "$RJC_HOOK_SSH_SHELL" ]; then shell_prefix=$RJC_HOOK_SSH_SHELL
elif [ -f devenv.yaml ] || [ -f devenv.nix ]; then shell_prefix="nix --extra-experimental-features nix-command --extra-experimental-features flakes run ` + DevenvNixpkgs + `#devenv -- shell --"; shell_managed=1
elif [ -f flake.nix ]; then shell_prefix="nix --extra-experimental-features nix-command --extra-experimental-features flakes develop -c"; shell_managed=1
else
  shell_prefix="nix --extra-experimental-features nix-command --extra-experimental-features flakes shell ` + Nixpkgs + `#pkl ` + Nixpkgs + `#git ` + Nixpkgs + `#nodejs_24 ` + Nixpkgs + `#pnpm ` + Nixpkgs + `#bun"
  if [ "$kind" != hk ]; then shell_prefix="$shell_prefix ` + Nixpkgs + `#$kind"; fi
  shell_prefix="$shell_prefix -c"
fi
`
