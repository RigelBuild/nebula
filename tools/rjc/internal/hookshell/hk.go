// Package hookshell builds the remote hook runner provisioning script.
package hookshell

import (
	"fmt"
	"strings"
)

// Nixpkgs pins remote nixpkgs references to a revision with a compatible pre-commit virtualenv.
const Nixpkgs = "github:NixOS/nixpkgs/b6018f87da91d19d0ab4cf979885689b469cdd41"

const hkVersion = "2.12.0"

// HKVersion is the hk release used by the hook runner.
const HKVersion = hkVersion

// hkPin describes one pinned static hk release.
type hkPin struct {
	host, platform, url, hash string
}

var hkPins = []hkPin{
	{"Linux x86_64", "x86_64-linux", "https://github.com/jdx/hk/releases/download/v${version}/hk-x86_64-unknown-linux-musl.tar.gz", "sha256-yrG6MMYFREEStEmXkKl70o3ag//DUrqkqofVq/sSxmg="},
	{"Linux aarch64", "aarch64-linux", "https://github.com/jdx/hk/releases/download/v${version}/hk-aarch64-unknown-linux-musl.tar.gz", "sha256-gnWLlnG0s2J/Em3I6YA7sVbCOJO0+ewhJ5f4HE0kLlA="},
	{"Darwin arm64", "aarch64-darwin", "https://github.com/jdx/hk/releases/download/v${version}/hk-aarch64-apple-darwin.tar.gz", "sha256-JpwX1H6ITBmtt3FE1ZV+hU/F/Goab5waClsZQx4vq7g="},
}

// Fetch returns a shell fragment that best-effort provisions the verified hk release.
//
//nolint:dupword // Shell control keywords are not repeated prose.
func Fetch() string {
	var script strings.Builder
	script.WriteString("    hk_dir=\n    hk_fetch() {\n      case \"$(uname -sm)\" in\n")
	for _, pin := range hkPins {
		fmt.Fprintf(&script, "        '%s') hk_url='%s'; hk_hash='%s'; hk_platform='%s' ;;\n", pin.host, strings.ReplaceAll(pin.url, "${version}", hkVersion), pin.hash, pin.platform)
	}
	script.WriteString(`        *) echo "unsupported host platform for static hk release: $(uname -sm)" >&2; return 1 ;;
      esac
      hk_cache="$HOME/rjc-hook/tools/hk-` + hkVersion + `-$hk_platform"
      if [ ! -x "$hk_cache/hk" ]; then
        hk_archive=$(nix --extra-experimental-features 'nix-command flakes' store prefetch-file --hash-type sha256 --expected-hash "$hk_hash" --json "$hk_url") || { echo "failed to prefetch hk release archive" >&2; return 1; }
        hk_archive=$(printf '%s\n' "$hk_archive" | sed -n 's/.*"storePath":[[:space:]]*"\([^\"]*\)".*/\1/p')
        if [ -z "$hk_archive" ] || [ ! -f "$hk_archive" ]; then echo "failed to prefetch hk release archive" >&2; return 1; fi
        mkdir -p "$HOME/rjc-hook/tools" || return 1
        hk_tmp=$(mktemp -d "$TMPDIR/hk.XXXXXX") || { echo "failed to create temporary hk release directory" >&2; return 1; }
        if ! tar -xzf "$hk_archive" -C "$hk_tmp"; then rm -rf "$hk_tmp"; echo "failed to unpack hk release archive" >&2; return 1; fi
        if [ ! -x "$hk_tmp/hk" ]; then rm -rf "$hk_tmp"; echo "hk release archive lacks an executable hk" >&2; return 1; fi
        if ! flock "$hk_cache.lock" sh -c 'if [ -e "$1" ]; then rm -rf "$2"; else mv "$2" "$1"; fi' sh "$hk_cache" "$hk_tmp"; then rm -rf "$hk_tmp"; echo "failed to publish hk release cache" >&2; return 1; fi
      fi
      if [ ! -x "$hk_cache/hk" ]; then echo "hk release cache lacks an executable hk" >&2; return 1; fi
      hk_dir="$hk_cache"
    }
    if ! hk_fetch; then echo "warning: could not provision static hk; checking selected shell" >&2; fi
    pkl_dir=$(nix --extra-experimental-features 'nix-command flakes' build --no-link --print-out-paths ` + Nixpkgs + `#pkl | sed 's#$#/bin#' | paste -sd: -)
    hook_dir="$hk_dir"
    if [ -n "$pkl_dir" ]; then hook_dir="${hook_dir:+$hook_dir:}$pkl_dir"; fi
`)
	return script.String()
}
