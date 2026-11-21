{
  description = "Read-only Cotal presence probe: seat presence and history-refusal holds as Prometheus gauges";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { nixpkgs, ... }:
    let
      # Per-system fixed-output hash of the bun dependency install. The aarch64
      # entries come from a real build on each system, never a guessed value.
      depsHash = {
        x86_64-linux = "sha256-9UknAR4DqXY/PfWVoHYrK5ur5uGOAw6lebl45iV1MOg=";
        aarch64-linux = "";
        aarch64-darwin = "";
      };
      # Released on all three release systems.
      systems = [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
          # Dependencies are a fixed-output derivation pinned by outputHash, the
          # same pattern as trunk-mq-mcp. Installs with the frozen lockfile and
          # no lifecycle scripts, so the output is reproducible.
          deps = pkgs.stdenvNoCC.mkDerivation {
            pname = "cotal-presence-probe-deps";
            version = "0.1.0";
            src = ./.;
            outputHash = depsHash.${system};
            outputHashMode = "recursive";
            outputHashAlgo = "sha256";
            nativeBuildInputs = [ pkgs.bun ];
            buildPhase = ''
              export HOME="$TMPDIR"
              bun install --frozen-lockfile --ignore-scripts
            '';
            installPhase = ''
              mkdir -p "$out"
              cp -R node_modules "$out/"
            '';
          };
          # bun build --compile appends its payload to a copy of bun's runtime.
          # The ELF links glibc and names a /nix/store interpreter, so postFixup
          # points it at the standard loader and drops the RPATH. The payload is
          # appended data that patchelf keeps, and dontStrip stops strip from
          # rewriting the file. CD's check-asset gate declares the NEEDED sonames
          # in component.cue.
          cotal-presence-probe = pkgs.stdenvNoCC.mkDerivation {
            pname = "cotal-presence-probe";
            version = "0.1.0";
            src = ./.;
            nativeBuildInputs = [ pkgs.bun pkgs.patchelf ];
            dontStrip = true;
            buildPhase = ''
              export HOME="$TMPDIR"
              cp -R ${deps}/node_modules ./node_modules
              bun build --compile --outfile cotal-presence-probe main.ts
            '';
            postFixup = ''
              patchelf --set-interpreter /lib64/ld-linux-x86-64.so.2 --remove-rpath "$out/bin/cotal-presence-probe"
            '';
            installPhase = ''
              install -Dm755 cotal-presence-probe "$out/bin/cotal-presence-probe"
            '';
          };
        in
        {
          inherit cotal-presence-probe;
          default = cotal-presence-probe;
        }
      );
    };
}
