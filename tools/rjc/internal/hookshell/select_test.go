package hookshell

import (
	"strings"
	"testing"
)

func TestSelectUsesCallerDevenvNixpkgs(t *testing.T) {
	const ref = "github:example/devenv-nixpkgs/1111111111111111111111111111111111111111"
	script := Select(ref)
	if !strings.Contains(script, ref+"#devenv") {
		t.Fatalf("shell selection does not use the caller devenv ref %q", ref+"#devenv")
	}
	if strings.Contains(script, Nixpkgs+"#devenv") {
		t.Fatalf("shell selection uses the shared nixpkgs pin for devenv")
	}
}
