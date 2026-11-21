{
  description = "direnv without its check phase, released prebuilt for hosts";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { nixpkgs, ... }:
    let
      systems = [ "x86_64-linux" "aarch64-darwin" ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
        in
        {
          # Matches orion's macRunnerOverlays: the upstream tests fail in the
          # darwin sandbox. cgo off keeps the Linux asset static.
          direnv = pkgs.direnv.overrideAttrs (old: {
            doCheck = false;
            env = (old.env or { }) // {
              CGO_ENABLED = "0";
            };
          });
        }
      );
    };
}
