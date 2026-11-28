{
  description = "Caddy with the Cloudflare DNS plugin, released prebuilt for hosts";

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
          throw "caddy ${pkg.version} does not match release.base ${base} in tools/caddy-cloudflare/component.cue";
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
        in
        {
          # The plugin set and hash a darwin host's Caddy uses. cgo off so the
          # Linux asset is static and passes CD's check-asset gate.
          caddy-cloudflare =
            ((requireVersion pkgs.caddy).withPlugins {
              plugins = [ "github.com/caddy-dns/cloudflare@v0.2.4" ];
              hash = "sha256-QSiwKqKvSxhksWBF4/MlBOpyXba5xOQ2sRwA0R7399E=";
            }).overrideAttrs
              (old: {
                env = (old.env or { }) // {
                  CGO_ENABLED = "0";
                };
              });
        }
      );
    };
}
