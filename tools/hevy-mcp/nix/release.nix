# The `hevy-mcp` CLI from the runtime tarball that nebula CD's `hevy-runtime` job publishes (.github/workflows/cd.yaml).
# Nothing is compiled: fetchurl pins the tarball by hash, and the install keeps the tree that job writes.
{ pkgs }:
let
  inherit (pkgs) lib;
  release = lib.importJSON ./release.json;
  system = pkgs.stdenv.hostPlatform.system;
  asset =
    release.assets.${system}
      or (throw "hevy-mcp: no published runtime asset for ${system} in nix/release.json");
  assetName = "hevy-mcp-runtime-${asset.platform}.tar.gz";
in
assert lib.assertMsg ((builtins.match "nebula-[0-9]{8}-[0-9]+" release.tag) != null)
  "tools/hevy-mcp/nix/release.json: tag ${release.tag} is not a nebula-YYYYMMDD-N release tag";
pkgs.stdenvNoCC.mkDerivation {
  pname = "hevy-mcp";
  inherit (release) version;

  src = pkgs.fetchurl {
    url = "https://github.com/${release.repo}/releases/download/${release.tag}/${assetName}";
    inherit (asset) hash;
  };

  # The archive has one top-level directory, `hevy-mcp/`: the npm package with its node_modules.
  sourceRoot = "hevy-mcp";

  nativeBuildInputs = [ pkgs.makeWrapper ];
  dontBuild = true;
  # Stripping would rewrite the prebuilt .node bindings, so keep them as released.
  dontStrip = true;

  installPhase = ''
    runHook preInstall

    mkdir -p "$out/lib" "$out/bin"
    cp -a . "$out/lib/hevy-mcp"
    makeWrapper ${pkgs.nodejs_24}/bin/node "$out/bin/hevy-mcp" \
      --add-flags "$out/lib/hevy-mcp/dist/cli.mjs"

    runHook postInstall
  '';

  meta = {
    description = "Hevy MCP server: workouts, routines, and body measurements over stdio";
    homepage = "https://github.com/chrisdoc/hevy-mcp";
    license = lib.licenses.mit;
    mainProgram = "hevy-mcp";
    platforms = builtins.attrNames release.assets;
    sourceProvenance = [ lib.sourceTypes.binaryNativeCode ];
  };
}
