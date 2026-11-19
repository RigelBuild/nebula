{
  description = "Read-only Trunk merge queue MCP server";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { nixpkgs, ... }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
          isLinux = pkgs.stdenv.hostPlatform.isLinux;
          # Dependencies are a fixed-output derivation pinned by outputHash, the
          # same pattern as docs-bundle in the nebula flake. Installs with the
          # frozen lockfile and no lifecycle scripts, so the output is reproducible.
          deps = pkgs.stdenvNoCC.mkDerivation {
            pname = "trunk-mq-mcp-deps";
            version = "0.1.0";
            src = ./.;
            outputHash = "sha256-jhpg8JFpqB+MDuTGEvcPEcv33lcHNGkVeHY8AhBaM1g=";
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
          # On Linux the ELF links glibc and names a /nix/store interpreter, so
          # postFixup points it at the standard loader and drops the RPATH. The
          # payload is appended data that patchelf keeps, and dontStrip stops
          # strip from rewriting the file. Darwin links only system libraries and
          # needs no change. CD's check-asset gate declares the NEEDED sonames in
          # component.cue.
          trunk-mq-mcp = pkgs.stdenvNoCC.mkDerivation {
            pname = "trunk-mq-mcp";
            version = "0.1.0";
            src = ./.;
            nativeBuildInputs = [ pkgs.bun ] ++ pkgs.lib.optional isLinux pkgs.patchelf;
            dontStrip = true;
            buildPhase = ''
              export HOME="$TMPDIR"
              cp -R ${deps}/node_modules ./node_modules
              bun build --compile --outfile trunk-mq-mcp bin/server.ts
            '';
            postFixup = pkgs.lib.optionalString isLinux ''
              interp=/lib64/ld-linux-x86-64.so.2
              if [ "${system}" = aarch64-linux ]; then interp=/lib/ld-linux-aarch64.so.1; fi
              patchelf --set-interpreter "$interp" --remove-rpath "$out/bin/trunk-mq-mcp"
            '';
            installPhase = ''
              install -Dm755 trunk-mq-mcp "$out/bin/trunk-mq-mcp"
            '';
          };
        in
        {
          inherit trunk-mq-mcp;
          default = trunk-mq-mcp;
        }
      );
    };
}
