{
  description = "sops-install-secrets with portable custom-path cleanup";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/d176bec6d3f7dc613eb18ae8cec9397610ef826f";
    sops-nix = {
      url = "github:Mic92/sops-nix/7c637afdccdf9bcf35c4a2d67b58a77c1fd843a4";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs =
    { nixpkgs, sops-nix, ... }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
      # The package version is upstream's 0.0.1. Its exact source revision is
      # locked above to the consumer's sops-nix input.
      base = builtins.head (builtins.match ''.*base: *"([^"]+)".*'' (builtins.readFile ./component.cue));
      requireVersion = pkg:
        if pkg.version == base then pkg
        else throw "sops-install-secrets ${pkg.version} does not match release.base ${base} in tools/sops-install-secrets/component.cue";
    in
    {
      packages = forAllSystems (
        system:
        let
          upstream = sops-nix.packages.${system}.sops-install-secrets;
        in
        {
          sops-install-secrets = (requireVersion upstream).overrideAttrs (old: {
            patches = (old.patches or [ ]) ++ [ ./sops-nix-custom-path-removals.patch ];
            env = (old.env or { }) // {
              CGO_ENABLED = "0";
            };
          });
        }
      );
    };
}
