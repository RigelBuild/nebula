{
  description = "Caddy with the Cloudflare DNS plugin, released prebuilt for hosts";

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
          # Same plugin and hash as orion's macmini nemo-dashboard. cgo off so
          # the Linux asset is static and passes CD's check-asset gate.
          caddy-cloudflare =
            (pkgs.caddy.withPlugins {
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
