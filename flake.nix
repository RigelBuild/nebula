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
      packagesFor = manifest: system:
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
            outputHash = "sha256-unItFE38ueLWvI/ZbHpNbjm9rqCGyYI0ScQTqXfa9/k=";
            buildPhase = ''
              export HOME="$TMPDIR"
              bun install --frozen-lockfile --ignore-scripts --production
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
        docsBundleFor system // packagesFor manifest system);

      # Eval-only check: a release-manifest entry must win over the source
      # build. The fixture is an attrset, so the check reads no file and
      # fetches nothing; it only reads the merged version attribute.
      checks = forAllSystems (system:
        let
          pkgs = import nixpkgs { inherit system; };
          fixture = {
            docs-bundle = {
              version = "9.9.9";
              upstreamRev = null;
              "${system}" = {
                url = "https://example.invalid/docs-bundle.tar.zst";
                sha256 = "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=";
              };
            };
          };
          merged = docsBundleFor system // packagesFor fixture system;
          expected = "9.9.9";
        in
        {
          manifest-wins = pkgs.runCommand "docs-bundle-manifest-wins" { } ''
            test "${merged.docs-bundle.version}" = "${expected}" || {
              echo "source build won over the release manifest" >&2
              exit 1
            }
            touch $out
          '';
        });

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
