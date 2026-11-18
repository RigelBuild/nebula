{
  description = "Nebula public monorepo packages and development tools";

  nixConfig = {
    extra-substituters = [
      "https://cache.nixos.org"
    ];
    extra-trusted-public-keys = [
      "cache.nixos.org-1:6NCHdD59X431o0gWypbMrAURkbJ16ZPMQFGspcDShjY="
    ];
    fallback = true;
  };

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
      manifest =
        if builtins.pathExists ./release-manifest.json then
          builtins.fromJSON (builtins.readFile ./release-manifest.json)
        else
          null;
      packagesFor = system:
        if manifest == null then { } else
          let
            pkgs = import nixpkgs { inherit system; };
            wrap = name: package:
              pkgs.stdenvNoCC.mkDerivation {
                pname = name;
                version = package.version;
                passthru.upstreamRev = package.upstreamRev or null;
                src = pkgs.fetchurl {
                  inherit (package.${system}) url sha256;
                };
                nativeBuildInputs = [ pkgs.zstd ];
                sourceRoot = ".";
                installPhase = ''
                  mkdir -p "$out/bin"
                  tar --use-compress-program=zstd -xf "$src" -C "$TMPDIR"
                  cp -R "$TMPDIR"/bin/. "$out/bin/"
                '';
              };
          in
          builtins.mapAttrs wrap (builtins.removeAttrs manifest [ "source" "tag" "held" ]);
      # docs-bundle is a TypeScript CLI run by Bun. Dependencies are fetched
      # as a fixed-output derivation pinned by outputHash, then the wrapper
      # runs cli.ts on the pinned Bun.
      docsBundleFor = system:
        let
          pkgs = import nixpkgs { inherit system; };
          docsBundleDeps = pkgs.stdenvNoCC.mkDerivation {
            pname = "docs-bundle-deps";
            version = "1.2.2";
            src = ./tools/docs-bundle;
            nativeBuildInputs = [ pkgs.bun ];
            outputHashMode = "recursive";
            outputHashAlgo = "sha256";
            outputHash = "sha256-4vilrxyUrqqNoFffVBJ6vjzsujoZavi3NpOEar7CPTY=";
            buildPhase = ''
              export HOME="$TMPDIR"
              bun install --frozen-lockfile --ignore-scripts
            '';
            installPhase = ''
              mkdir -p "$out"
              cp -R node_modules "$out/"
            '';
          };
          docsBundle = pkgs.stdenvNoCC.mkDerivation {
            pname = "docs-bundle";
            version = "1.2.2";
            src = ./tools/docs-bundle;
            nativeBuildInputs = [ pkgs.makeWrapper ];
            installPhase = ''
              runHook preInstall
              mkdir -p "$out/lib/docs-bundle" "$out/bin"
              cp -R . "$out/lib/docs-bundle"
              cp -R ${docsBundleDeps}/node_modules "$out/lib/docs-bundle/"
              makeWrapper ${pkgs.bun}/bin/bun "$out/bin/docs-bundle" \
                --add-flags "$out/lib/docs-bundle/cli.ts"
              runHook postInstall
            '';
          };
        in
        { "docs-bundle" = docsBundle; };
    in
    {
      packages = forAllSystems (system:
        packagesFor system // docsBundleFor system);

      devShells = forAllSystems (system:
        let
          pkgs = import nixpkgs { inherit system; };
        in
        {
          default = pkgs.mkShell {
            packages = with pkgs; [ go bun moon rumdl biome ];
          };
        });
    };
}
