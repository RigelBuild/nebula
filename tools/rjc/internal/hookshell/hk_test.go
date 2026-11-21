package hookshell

import (
	"os"
	"regexp"
	"testing"
)

func TestReleasePinsMatchNix(t *testing.T) {
	data, err := os.ReadFile("../../../../ci/toolchain/versions/hk.nix")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	version := regexp.MustCompile(`version\s*=\s*"([^"]+)"`).FindStringSubmatch(source)
	if len(version) != 2 || version[1] != hkVersion {
		t.Fatalf("Go hk version %q does not match Nix pin %q", hkVersion, version)
	}
	matches := regexp.MustCompile(`(?s)"([^"]+)"\s*=\s*\{\s*url\s*=\s*"([^"]+)";\s*hash\s*=\s*"([^"]+)";\s*\};`).FindAllStringSubmatch(source, -1)
	if len(matches) != len(hkPins) {
		t.Fatalf("Nix pin has %d system releases, Go has %d", len(matches), len(hkPins))
	}
	for _, pin := range hkPins {
		found := false
		for _, match := range matches {
			if match[1] == pin.platform && match[2] == pin.url && match[3] == pin.hash {
				found = true
			}
		}
		if !found {
			t.Errorf("Go pin for %s does not match Nix", pin.platform)
		}
	}
}
