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
        cotal-sys-creds-check = null;
        design-ledger-gate = "sha256-euBbY7ewPBNIwVPjPkAX9DVbH6XaJ81n+5aMJfBzPW8=";
        dev-hygiene = null;
        depsupdate-wake = null;
        dl-claim = "sha256-3EcIygo4CNKJmMCIWrIL/bFMqBUznQCV9HI6h1EyeGU=";
        dl-list = "sha256-+YGOcD3K7shRymY29qRJa49GfFkOc8wsdeMdmpvv44A=";
        dl-reconcile = "sha256-M9sG3Wt4un2pQ17tcrLI+K/u1K8JjXy2DXyGrGU8emk=";
        fork-sync = "sha256-3CiEdm8Db78vQ8JG1KOBt5w55pmxuTs04in8RUhzwoM=";
        gh-route = "sha256-7K17JaXFsjf163g5PXCb5ng2gYdotnZ2IDKk8KFjNj0=";
        hk-hook = null;
        host-secret-ready = null;
        length-gate = "sha256-m4lHVyrsYIrQpPFIblIZcMc5rPmJJJsHt6RS8QAoXCk=";
        linear-acceptance = null;
        longhorn-drain = null;
        rbuild-workspace-prune = null;
        reddit-fetch = null;
        ref-gate = "sha256-WVoZkg5kdUYlATXDDxAA/yozThBwyYSKHwLoUsuIBfg=";
        rjc-gates-image = "sha256-WJ+DzF3TydFNjIOq2MAQxEDI62G8xJcjTCAyMZtwgP0=";
        ssh-orphan-reap = null;
        wave-host = "sha256-3/tNPMYKxKjwprceoPRjRJuTNBwJSJE4LD8grnh+haI=";
      };

      # Does a command's go.mod replace the shared go-lib module? The replace
      # target is relative to tools/<name>; it counts only if it resolves to
      # exactly tools/go-lib. A commented or longer path does not count.
      usesGoLibFor = name: gomod:
        let
          resolve = target:
            lib.concatStringsSep "/" (lib.foldl' (acc: p:
              if p == ".." then lib.init acc else if p == "." then acc else acc ++ [ p ])
              [ ] (lib.splitString "/" "tools/${name}/${target}"));
          replaceTargets = map (line: lib.last (lib.splitString " => " line))
            (lib.filter (line: lib.hasPrefix "replace " line && lib.hasInfix " => " line)
              (lib.splitString "\n" gomod));
        in
        lib.any (target: resolve target == "tools/go-lib") replaceTargets;

      # Source for one command: its own directory, plus tools/go-lib when its
      # go.mod replaces the shared module.
      commandSource = name:
        let
          usesGoLib = usesGoLibFor name (builtins.readFile (./tools + "/${name}/go.mod"));
        in
        lib.fileset.toSource {
          root = ./.;
          fileset = lib.fileset.unions ([
            (./tools + "/${name}")
          ] ++ lib.optional usesGoLib ./tools/go-lib);
        };

      # checkTools are extra binaries a command's tests look up on PATH.
      # code-shape-gate's tests run the Go and TypeScript linters and require
      # exact versions (codeshape/toolversion.go). Fail eval when nixpkgs
      # drifts from them, so a nix flake update cannot break the check phase
      # silently. The input to pin is nixpkgs (nixpkgs.url in this flake).
      checkTools = {
        code-shape-gate = pkgs:
          let
            requireVersion = pkg: expected:
              if pkg.version == expected then pkg
              else throw ''
                ${pkg.pname} ${pkg.version} does not match the ${expected} that
                tools/code-shape-gate/codeshape/toolversion.go requires.
                Pin nixpkgs (inputs.nixpkgs.url in flake.nix) to a revision
                that ships ${pkg.pname} ${expected}, or update toolversion.go
                and this check together.
              '';
          in
          [
            (requireVersion pkgs.golangci-lint "2.15.0")
            (requireVersion pkgs.biome "2.6.5")
          ];
        # dev-hygiene's workspace tests create real jj repositories as fixtures.
        dev-hygiene = pkgs: [ pkgs.jujutsu ];
        # hk-hook's tests create jj workspaces with jj on PATH.
        hk-hook = pkgs: [ pkgs.jujutsu ];
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

      # mainProgram defaults to the package name. Name the binary here when it differs.
      mainPrograms = {
        caddy-cloudflare = "caddy";
        jj-hooks-bin = "jj-hp";
      };

      packagesFor = manifest: system:
        if manifest == null then { } else
          let
            pkgs = import nixpkgs { inherit system; };
            wrap = name: package:
              pkgs.stdenvNoCC.mkDerivation {
                # Distinct from the source build's name, so consumers can tell a fetch from a compile.
                pname = "${name}-prebuilt";
                version = package.version;
                passthru.upstreamRev = package.upstreamRev or null;
                meta.mainProgram = mainPrograms.${name} or name;
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
          builtins.mapAttrs wrap (lib.filterAttrs (_: p: p ? ${system}) (builtins.removeAttrs manifest [ "source" "tag" "held" "images" ]));

      # docs-bundle is a TypeScript CLI run by Bun. Dependencies are fetched
      # as a fixed-output derivation pinned by outputHash, then the wrapper
      # runs cli.ts on the pinned Bun. Installs production deps only, so the
      # output is the same on every system.
      docsBundleFor = system:
        let
          pkgs = import nixpkgs { inherit system; };
          # The build reads ../tsconfig/base.json through tools/docs-bundle/tsconfig.json,
          # so the source carries both trees and the build runs in tools/docs-bundle.
          # The packaged tsconfig.json's extends does not resolve; harmless, since nothing runs tsc on the package.
          docsBundleSource = lib.fileset.toSource {
            root = ./.;
            fileset = lib.fileset.unions [
              ./tools/docs-bundle
              ./tools/tsconfig
            ];
          };
          docsBundleDeps = pkgs.stdenvNoCC.mkDerivation {
            pname = "docs-bundle-deps";
            version = "1.2.2";
            src = docsBundleSource;
            # Unpacks to ./tools/{docs-bundle,tsconfig}; the build runs in tools/docs-bundle.
            nativeBuildInputs = [ pkgs.bun ];
            outputHashMode = "recursive";
            outputHashAlgo = "sha256";
            outputHash = "sha256-unItFE38ueLWvI/ZbHpNbjm9rqCGyYI0ScQTqXfa9/k=";
            buildPhase = ''
              cd tools/docs-bundle
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
            src = docsBundleSource;
            # Unpacks to ./tools/{docs-bundle,tsconfig}; the build runs in tools/docs-bundle.
            nativeBuildInputs = [ pkgs.makeWrapper ];
            installPhase = ''
              runHook preInstall
              cd tools/docs-bundle
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

      # Static on Linux so the asset passes CD's check-asset gate (no /nix/store
      # interpreter). A released manifest entry of the same name overrides it.
      nix2containerFor =
        system:
        let
          pkgs = import nixpkgs { inherit system; };
          upstream = builtins.fromJSON (builtins.readFile ./forks/nix2container/upstream.json);
          target = if pkgs.stdenv.hostPlatform.isLinux then pkgs.pkgsStatic else pkgs;
          bin = (import ./forks/nix2container { pkgs = target; }).nix2container-bin;
        in
        {
          nix2container = bin.overrideAttrs (old: {
            # cgo off, as the Go commands above: the asset links no cgo libraries.
            env = (old.env or { }) // {
              CGO_ENABLED = "0";
            };
            version = "1.1.0-rigel.2";
            passthru = (old.passthru or { }) // { upstreamRev = upstream.commit; };
          });
        };

      # Shared markdown policy that consumers extend with
      # `extends = "$RUMDL_BASE_CONFIG"`. The file sits at rumdl/base.toml under
      # the package root. It is a flake output read by path, not a Release asset.
      rumdlBaseConfigFor =
        system:
        let
          pkgs = import nixpkgs { inherit system; };
        in
        {
          rumdl-base-config = pkgs.runCommand "rumdl-base-config" { } ''
            mkdir -p "$out/rumdl"
            cp ${./tools/rumdl-base-config/base.toml} "$out/rumdl/base.toml"
          '';
        };
    in
    {
      # Later attrs win. Source builds come first, then docs-bundle and
      # nix2container, and released manifest packages last, so a release
      # overrides the source build of the same name.
      packages = forAllSystems (
        system:
        commandPackagesFor system
        // docsBundleFor system
        // nix2containerFor system
        // rumdlBaseConfigFor system
        // packagesFor manifest system
      );

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
            nix2container = {
              version = "9.9.9";
              upstreamRev = null;
              "${system}" = {
                url = "https://example.invalid/nix2container.tar.zst";
                sha256 = "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=";
              };
            };
            jj-hooks-bin = {
              version = "9.9.9";
              upstreamRev = null;
              "${system}" = {
                url = "https://example.invalid/jj-hooks-bin.tar.zst";
                sha256 = "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=";
              };
            };
            caddy-cloudflare = {
              version = "9.9.9";
              upstreamRev = null;
              "${system}" = {
                url = "https://example.invalid/caddy-cloudflare.tar.zst";
                sha256 = "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=";
              };
            };
            # Image records are not packages; packagesFor must skip them.
            images = {
              omp = {
                repository = "ghcr.io/rigelbuild/omp";
                tag = "nebula-20261117-1";
                digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000";
              };
            };
          };
          merged =
            commandPackagesFor system
            // docsBundleFor system
            // nix2containerFor system
            // packagesFor fixture system;
          expected = "9.9.9";
        in
        {
          manifest-wins = pkgs.runCommand "nebula-manifest-wins" { } ''
            test "${merged.docs-bundle.version}" = "${expected}" || {
              echo "source build won over the release manifest" >&2
              exit 1
            }
            test "${merged.nix2container.version}" = "${expected}" || {
              echo "nix2container source build won over the release manifest" >&2
              exit 1
            }
            touch $out
          '';
          # The wrapper must not take a source package's name: a consumer host's
          # check fails on a derivation named <fork>-<digit>...
          manifest-prebuilt-name = pkgs.runCommand "nebula-manifest-prebuilt-name" { } ''
            test "${merged.docs-bundle.name}" = "docs-bundle-prebuilt-${expected}" || {
              echo "the manifest wrapper kept the source package name" >&2
              exit 1
            }
            test "${merged.docs-bundle.meta.mainProgram}" = "docs-bundle" || {
              echo "the manifest wrapper has no meta.mainProgram" >&2
              exit 1
            }
            test "${merged.jj-hooks-bin.meta.mainProgram}" = "jj-hp" || {
              echo "the jj-hooks-bin wrapper has the wrong meta.mainProgram" >&2
              exit 1
            }
            test "${merged.caddy-cloudflare.meta.mainProgram}" = "caddy" || {
              echo "the caddy-cloudflare wrapper has the wrong meta.mainProgram" >&2
              exit 1
            }
            touch $out
          '';
          # A manifest with images evaluates, and images is not a package.
          manifest-images-skipped = pkgs.runCommand "nebula-manifest-images-skipped" { } ''
            test "${builtins.toJSON (builtins.hasAttr "images" (packagesFor fixture system))}" = false || {
              echo "the manifest images key became a package" >&2
              exit 1
            }
            touch $out
          '';
          # A package released for one system only is absent on the others, not an
          # eval error. Without the filter in packagesFor, aarch64-linux throws on
          # trunk-mq-mcp, which has no aarch64-linux asset.
          manifest-single-system =
            let
              singleFixture = {
                source = "abc";
                tag = "nebula-20261117-44";
                held = [ ];
                trunk-mq-mcp = {
                  version = "0.1.0";
                  upstreamRev = null;
                  "x86_64-linux" = {
                    url = "https://example.invalid/trunk-mq-mcp.tar.zst";
                    sha256 = "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=";
                  };
                };
              };
              onOther = builtins.attrNames (packagesFor singleFixture "aarch64-linux");
            in
            pkgs.runCommand "nebula-manifest-single-system" { } ''
              test '${builtins.toJSON onOther}' = '[]' || {
                echo "aarch64-linux listed a package with no aarch64-linux asset" >&2
                exit 1
              }
              touch $out
            '';
          # A command that replaces go-lib must ship tools/go-lib in its source.
          go-lib-included = pkgs.runCommand "nebula-go-lib-included" { } ''
            test -f "${commandSource "dl-claim"}/tools/go-lib/go.mod" || {
              echo "dl-claim replaces go-lib but tools/go-lib is missing from its source" >&2
              exit 1
            }
            touch $out
          '';
          # usesGoLibFor must reject a commented replace and accept a live one.
          # Evaluated through the same function commandSource uses.
          go-lib-detection = pkgs.runCommand "nebula-go-lib-detection" { } ''
            ${if usesGoLibFor "fixture" "# replace github.com/x/go-lib => ../go-lib\n" then ''
              echo "a commented replace was accepted as go-lib" >&2
              exit 1
            '' else ""}
            ${if usesGoLibFor "fixture" "replace github.com/x/go-lib => ../go-lib\n" then "" else ''
              echo "a live replace to ../go-lib was not accepted" >&2
              exit 1
            ''}
            touch $out
          '';
          nix2container-attrs =
            let
              names = builtins.attrNames (self.lib.nix2container { inherit pkgs; });
              expected = [ "buildImage" "buildLayer" "pullImage" "pullImageFromManifest" ];
            in
            pkgs.runCommand "nix2container-attrs" { } ''
              test '${builtins.toJSON names}' = '${builtins.toJSON expected}' || {
                echo "lib.nix2container attribute names changed" >&2
                exit 1
              }
              touch $out
            '';
        });

      # Nix libraries. lib.nix2container takes the caller's pkgs and returns the
      # attrset upstream exposes as packages.<system>.nix2container. It has no
      # per-system form: the caller's pkgs picks the system.
      lib.nix2container = { pkgs }:
        (import ./forks/nix2container { inherit pkgs; }).nix2container;

      devShells = forAllSystems (system:
        let
          pkgs = import nixpkgs { inherit system; };
          basePackages = with pkgs; [ go bun moon rumdl biome cargo rustc clippy rustfmt cargo-nextest golangci-lint jujutsu nodejs_26 pnpm nats-server ];
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
