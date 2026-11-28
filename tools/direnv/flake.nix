{
  description = "direnv without its check phase, released prebuilt for hosts";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { nixpkgs, ... }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
      # release.base in component.cue. Eval fails when nixpkgs drifts from it,
      # so a nix flake update cannot release a mislabeled asset.
      base = builtins.head (builtins.match ''.*base: *"([^"]+)".*'' (builtins.readFile ./component.cue));
      requireVersion =
        pkg:
        if pkg.version == base then
          pkg
        else
          throw "direnv ${pkg.version} does not match release.base ${base} in tools/direnv/component.cue";
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
        in
        {
          # The upstream tests fail in the darwin build sandbox, so a darwin
          # host turns them off. The released binary must not name /nix/store:
          # an empty BASH_PATH (the buildPhase passes it to make) leaves
          # main.bashPath unset, so direnv finds bash on PATH at run time.
          # Upstream nixpkgs already builds it static with cgo off.
          direnv = (requireVersion pkgs.direnv).overrideAttrs (old: {
            doCheck = false;
            env = (old.env or { }) // {
              BASH_PATH = "";
            };
          });
        }
      );
    };
}
