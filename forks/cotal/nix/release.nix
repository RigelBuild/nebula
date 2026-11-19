# The `cotal` CLI from the runtime tarball that nebula CD's `runtime` job publishes (.github/workflows/cd.yaml).
# Nothing is compiled: fetchurl pins the tarball by hash, and the install keeps the tree layout that job writes.
{ pkgs }:
let
  inherit (pkgs) lib;
  release = lib.importJSON ./release.json;
  system = pkgs.stdenv.hostPlatform.system;
  asset =
    release.assets.${system}
      or (throw "cotal: no published runtime asset for ${system} in nix/release.json");
  assetName = "cotal-runtime-${asset.platform}.tar.gz";
in
assert lib.assertMsg ((builtins.match "nebula-[0-9]{8}-[0-9]+" release.tag) != null)
  "nix/release.json: tag ${release.tag} is not a nebula-YYYYMMDD-N release tag";
pkgs.stdenvNoCC.mkDerivation {
  pname = "cotal";
  inherit (release) version;

  src = pkgs.fetchurl {
    url = "https://github.com/${release.repo}/releases/download/${release.tag}/${assetName}";
    inherit (asset) hash;
  };

  # The archive has one top-level directory, `cotal/`, which holds the runtime tree.
  sourceRoot = "cotal";

  nativeBuildInputs = [ pkgs.makeWrapper ];
  dontBuild = true;
  # Stripping would rewrite the prebuilt .node files, so keep them as released.
  dontStrip = true;

  installPhase = ''
    runHook preInstall

    mkdir -p "$out/lib" "$out/bin"
    cp -a . "$out/lib/cotal"

    # The runtime job copies no other pnpm workspace members (desktop, tools/docs-content), so links to them dangle.
    # Drop only those links; keep the noBrokenSymlinks fixup, which still guards every other link.
    find "$out/lib/cotal" -xtype l -delete

    makeWrapper ${pkgs.nodejs_24}/bin/node "$out/bin/cotal" \
      --add-flags "$out/lib/cotal/cotal-ai/dist/cotal.js"

    runHook postInstall
  '';

  meta = {
    description = "Cotal mesh CLI and built-in extension packages from the nebula runtime tarball";
    homepage = "https://github.com/${release.repo}";
    license = lib.licenses.asl20;
    mainProgram = "cotal";
    platforms = builtins.attrNames release.assets;
    sourceProvenance = [ lib.sourceTypes.binaryNativeCode ];
  };
}
