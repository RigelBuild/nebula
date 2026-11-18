{
  description = "jj-vine, installed from the RigelBuild/jj-vine release binaries";

  inputs.nixpkgs.url = "github:nixos/nixpkgs/nixos-26.05";

  outputs =
    { nixpkgs, ... }:
    let
      inherit (nixpkgs) lib;

      version = "0.5.4-rigel.3";

      # One upstream release asset per system. Each sha256 is bare hex, copied
      # from the asset as published on the v${version} GitHub release.
      artifacts = {
        aarch64-darwin = {
          target = "aarch64-apple-darwin";
          sha256 = "f33e3d2869575e8ea769a9a374a95b0ef87c04850d204ac8a08b2aa13106cbb5";
        };
        aarch64-linux = {
          target = "aarch64-unknown-linux-musl";
          sha256 = "9f7e4e6947817013034d7d3cc8ce75b361d87bc6392b59ad7a421576637215a0";
        };
        x86_64-linux = {
          target = "x86_64-unknown-linux-musl";
          sha256 = "63264e343ea405cf6979aa159079ce057fb1e3079c87d7b25f24f5af12c8b5f0";
        };
      };

      systems = builtins.attrNames artifacts;
      forAllSystems = lib.genAttrs systems;

      # NOT rustPlatform.buildRustPackage: install the binary upstream built, so
      # no host compiles jj-vine at switch time and no Rust inputs enter the
      # closure. The release archive holds `jj-vine` at its root, so point the
      # build at the unpack dir itself.
      mkJjVine =
        pkgs: artifact:
        pkgs.stdenvNoCC.mkDerivation {
          pname = "jj-vine";
          inherit version;
          src = pkgs.fetchurl {
            url = "https://github.com/RigelBuild/jj-vine/releases/download/v${version}/jj-vine-${version}-${artifact.target}.tar.gz";
            inherit (artifact) sha256;
          };
          sourceRoot = ".";
          # Do NOT add strip: it would rewrite the upstream binary, and the
          # darwin arm64 build needs its ad-hoc code signature intact to exec.
          installPhase = ''
            runHook preInstall
            install -Dm755 jj-vine "$out/bin/jj-vine"
            runHook postInstall
          '';
          meta = {
            description = "Stacked pull requests for jj (jujutsu)";
            homepage = "https://codeberg.org/abrenneke/jj-vine";
            license = pkgs.lib.licenses.mit;
            mainProgram = "jj-vine";
            platforms = systems;
          };
        };

      # The portable release build nebula CD ships as the jj-vine asset. A
      # release asset must run without this build's /nix/store: on Linux,
      # pkgsStatic makes a static musl ELF; on darwin, which cannot link fully
      # static, it links only the system libSystem under /usr/lib. CD's
      # check-asset gate enforces both rules.
      mkJjVineStatic =
        pkgs:
        let
          target = if pkgs.stdenv.hostPlatform.isLinux then pkgs.pkgsStatic else pkgs;
        in
        target.rustPlatform.buildRustPackage (
          {
            pname = "jj-vine-static";
            inherit version;
            src = lib.fileset.toSource {
              root = ./.;
              fileset = lib.fileset.unions [
                ./Cargo.toml
                ./Cargo.lock
                ./README.md
                ./src
              ];
            };
            cargoLock.lockFile = ./Cargo.lock;
            # aws-lc-sys builds its C sources with cmake.
            nativeBuildInputs = [
              pkgs.cmake
            ]
            ++ lib.optional pkgs.stdenv.hostPlatform.isDarwin pkgs.darwin.autoSignDarwinBinariesHook;
            # The suite needs jj and forge fixtures; this build only smoke-tests.
            doCheck = false;
            doInstallCheck = true;
            installCheckPhase = ''
              runHook preInstallCheck
              "$out/bin/jj-vine" --version >/dev/null
            ''
            + lib.optionalString pkgs.stdenv.hostPlatform.isDarwin ''
              # Fail here, not at check-asset, if a dylib comes from the store.
              if otool -L "$out/bin/jj-vine" | tail -n +2 | grep -q /nix/store; then
                otool -L "$out/bin/jj-vine"
                exit 1
              fi
            ''
            + ''
              runHook postInstallCheck
            '';
            meta = {
              description = "Portable jj-vine release binary";
              homepage = "https://codeberg.org/abrenneke/jj-vine";
              license = lib.licenses.mit;
              mainProgram = "jj-vine";
            };
          }
          // lib.optionalAttrs pkgs.stdenv.hostPlatform.isDarwin {
            # Darwin only: the stdenv's libiconv (propagated by apple-sdk) has a
            # /nix/store install name that libc's #[link(name = "iconv")] pulls in.
            # Repoint it at the system dylib (dyld shared cache, same ABI). The
            # auto-sign hook re-signs in postFixupHooks; installCheck only checks.
            postFixup = ''
              install_name_tool -change "${pkgs.darwin.libiconv}/lib/libiconv.2.dylib" \
                /usr/lib/libiconv.2.dylib "$out/bin/jj-vine"
            '';
          }
        );

      packages = forAllSystems (system: {
        jj-vine = mkJjVine nixpkgs.legacyPackages.${system} artifacts.${system};
        jj-vine-static = mkJjVineStatic nixpkgs.legacyPackages.${system};
      });
    in
    {
      inherit packages;

      # Evaluates each package's derivation without realising it. The drv path
      # is written out as text with its string context dropped, so the check
      # neither depends on nor builds the package.
      checks = forAllSystems (system: {
        jj-vine = nixpkgs.legacyPackages.${system}.writeText "jj-vine-drv-path" (
          builtins.unsafeDiscardStringContext packages.${system}.jj-vine.drvPath
        );
      });
    };
}
