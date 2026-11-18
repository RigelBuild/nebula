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
      lib = nixpkgs.lib;
      forAllSystems = lib.genAttrs systems;

      # Each first-party Go command is its own module at tools/<name>/. The
      # value is that module's vendorHash.
      goCommands = {
        code-shape-gate = "sha256-Ee6lA9fZjXfNx2tkoScY+Su5JULWUpK4DshrXO3fmkU=";
        design-ledger-gate = "sha256-euBbY7ewPBNIwVPjPkAX9DVbH6XaJ81n+5aMJfBzPW8=";
        dl-claim = "sha256-3EcIygo4CNKJmMCIWrIL/bFMqBUznQCV9HI6h1EyeGU=";
        dl-list = "sha256-+YGOcD3K7shRymY29qRJa49GfFkOc8wsdeMdmpvv44A=";
        dl-reconcile = "sha256-M9sG3Wt4un2pQ17tcrLI+K/u1K8JjXy2DXyGrGU8emk=";
        fork-sync = "sha256-3CiEdm8Db78vQ8JG1KOBt5w55pmxuTs04in8RUhzwoM=";
        length-gate = "sha256-m4lHVyrsYIrQpPFIblIZcMc5rPmJJJsHt6RS8QAoXCk=";
        ref-gate = "sha256-WVoZkg5kdUYlATXDDxAA/yozThBwyYSKHwLoUsuIBfg=";
      };

      # Source for one command: its own directory, plus tools/go-lib when its
      # go.mod replaces the shared module.
      commandSource = name:
        let
          usesGoLib = lib.hasInfix "=> ../go-lib"
            (builtins.readFile (./tools + "/${name}/go.mod"));
        in
        lib.fileset.toSource {
          root = ./.;
          fileset = lib.fileset.unions ([
            (./tools + "/${name}")
          ] ++ lib.optional usesGoLib ./tools/go-lib);
        };

      # checkTools are extra binaries a command's tests look up on PATH.
      # code-shape-gate's tests run the Go and TypeScript linters and check
      # their pinned versions (golangci-lint 2.15.0, biome 2.6.5).
      checkTools = {
        code-shape-gate = pkgs: [ pkgs.golangci-lint pkgs.biome pkgs.go ];
      };

      buildCommand = pkgs: name: vendorHash:
        pkgs.buildGoModule {
          pname = name;
          version = self.shortRev or self.dirtyShortRev or "dirty";
          src = commandSource name;
          modRoot = "tools/${name}";
          inherit vendorHash;
          # Several command tests run git against temporary repositories.
          nativeCheckInputs = [ pkgs.git ] ++ (checkTools.${name} or (_: [ ])) pkgs;
          # golangci-lint writes its build cache under HOME, which the sandbox
          # leaves unwritable. Give the check phase a writable HOME.
          preCheck = ''
            export HOME="$TMPDIR/home"
            mkdir -p "$HOME"
          '';
          env.CGO_ENABLED = "0";
          ldflags = [ "-s" "-w" ];
        };

      commandPackagesFor = system:
        let
          pkgs = import nixpkgs { inherit system; };
        in
        builtins.mapAttrs (buildCommand pkgs) goCommands;

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
      # runs cli.ts on the pinned Bun. Installs production deps only, so the
      # output is the same on every system.
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
      # Later attrs win. Source builds come first, docs-bundle next, and
      # released manifest packages last, so a release overrides the source
      # build of the same name.
      packages = forAllSystems (system:
        commandPackagesFor system // docsBundleFor system // packagesFor manifest system);

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
          merged = commandPackagesFor system // docsBundleFor system // packagesFor fixture system;
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
          basePackages = with pkgs; [ go bun moon rumdl biome cargo rustc clippy rustfmt cargo-nextest golangci-lint jujutsu ];
          hookPackages = with pkgs; [ pre-commit prek lefthook pkl hk ];
        in
        {
          default = pkgs.mkShell {
            packages = basePackages;
          };
          # The jj-hooks test drives the hook runners; the default shell stays lean.
          jj-hooks = pkgs.mkShell {
            packages = basePackages ++ hookPackages;
          };
        });
    };
}
