# The bare Bun-compiled ELF for CD's kind: nix artifact (RIG-11694).
#
# The omp derivation's postFixup wraps bin/omp: makeBinaryWrapper moves the ELF
# to .omp-wrapped and adds DT_NEEDED libstdc++.so.6. This overrides that
# postFixup, so bin/omp is the Bun output itself. It sets the standard loader
# for the machine, drops RUNPATH, and repairs DT_VERDEF as linux-loader-fixup.nix
# does. dontAutoPatchelf keeps the autoPatchelf hook from rewriting the
# interpreter or RUNPATH after this patch. Darwin's omp is already a bare
# Mach-O, so it is returned unchanged.
#
# No install check runs here: the build sandbox has no FHS loader at
# /lib64, so the bare ELF cannot execute inside it. It is run on a host
# instead (see the RIG-11694 evidence).
#
# The embedded addon needs glibc 2.34+. check-asset reads only the outer ELF,
# so keep the embedded addon's needs in step with component.cue by hand.
{
  bun,
  lib,
  omp,
  stdenv,
}:
let
  standardLoader =
    if stdenv.hostPlatform.isx86_64 then
      "/lib64/ld-linux-x86-64.so.2"
    else if stdenv.hostPlatform.isAarch64 then
      "/lib/ld-linux-aarch64.so.1"
    else
      throw "omp-release: no standard loader for ${stdenv.hostPlatform.system}";
in
if stdenv.hostPlatform.isLinux then
  omp.overrideAttrs (_old: {
    dontAutoPatchelf = true;
    postFixup = ''
      patchelf --set-interpreter ${standardLoader} --remove-rpath "$out/bin/omp"
      # Repair DT_VERDEF after patchelf; stale offsets SIGSEGV before main() (#9881).
      ${lib.getExe bun} ${../scripts/fix-dt-verdef.ts} "$out/bin/omp"
      # check-asset reads only the outer ELF, so a kept addon RUNPATH would pass it.
      rpath="$(cat "$out/nix-support/embedded-addon-runpath")"
      [ -z "$rpath" ] || { echo "omp-release: embedded addon kept RUNPATH $rpath" >&2; exit 1; }
    '';
    # The inherited preInstallCheck repairs .omp-wrapped, which this build does not produce.
    preInstallCheck = "";
    doInstallCheck = false;
  })
else
  omp
