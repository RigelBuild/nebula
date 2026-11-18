// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
)

const (
	testReleaseURL = "https://github.com/RigelBuild/nebula/releases/download/nebula-20261117-42/"
	sumDarwinArm   = "a505398e46c24674411da7e2d358fccfc78347e7da517e64792e91c69072ec43"
	sumLinuxIntel  = "99f442f388dd9ee416d52a83ced8fea0fe8cdc6d28b02559859d170de9940474"
	sumLinuxArm    = "6ff832c68de996ae9181d60b3d9b69d5275d2d2190ccd82b1b633784dff622c3"
)

func lengthGatePackage() Package {
	return Package{Version: "0.4.0", Systems: map[string]Asset{
		"aarch64-darwin": {URL: testReleaseURL + "length-gate-0.4.0-aarch64-darwin.tar.zst", SHA256: sumDarwinArm},
		"x86_64-linux":   {URL: testReleaseURL + "length-gate-0.4.0-x86_64-linux.tar.zst", SHA256: sumLinuxIntel},
		"aarch64-linux":  {URL: testReleaseURL + "length-gate-0.4.0-aarch64-linux.tar.zst", SHA256: sumLinuxArm},
	}}
}

func TestRenderFormulaMatchesGolden(t *testing.T) {
	got, err := renderFormula("length-gate", lengthGatePackage())
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "length-gate.rb"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("formula differs from testdata/length-gate.rb:\n%s", got)
	}
}

// TestFormulaInstallMatchesAssetLayout builds an asset the way the batch step does
// (tar -C stage -cf - bin), unpacks it the way Homebrew does, and checks that the
// formula's install glob finds the binary from inside the unpacked directory.
// Homebrew enters the archive's single top-level directory, so the glob is
// relative to bin/, not to the archive root.
func TestFormulaInstallMatchesAssetLayout(t *testing.T) {
	stage := t.TempDir()
	writeFile(t, stage, "bin/length-gate", "binary\n")
	archive := filepath.Join(t.TempDir(), "length-gate-0.4.0-x86_64-linux.tar")
	if out, err := exec.Command("tar", "-C", stage, "-cf", archive, "bin").CombinedOutput(); err != nil {
		t.Fatalf("tar: %v: %s", err, out)
	}
	unpacked := t.TempDir()
	if out, err := exec.Command("tar", "-C", unpacked, "-xf", archive).CombinedOutput(); err != nil {
		t.Fatalf("untar: %v: %s", err, out)
	}
	entries, err := os.ReadDir(unpacked)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		t.Fatalf("archive top level = %v, want one directory", entries)
	}
	installDir := filepath.Join(unpacked, entries[0].Name())

	formula, err := renderFormula("length-gate", lengthGatePackage())
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`bin\.install Dir\["([^"]+)"\]`).FindSubmatch(formula)
	if match == nil {
		t.Fatalf("formula has no bin.install Dir[...] line:\n%s", formula)
	}
	found, err := filepath.Glob(filepath.Join(installDir, string(match[1])))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range found {
		names = append(names, filepath.Base(f))
	}
	if !reflect.DeepEqual(names, []string{"length-gate"}) {
		t.Fatalf("install glob %q in %s found %v, want [length-gate]", match[1], installDir, names)
	}
}

func TestWriteFormulasWritesOnlyBinaryTools(t *testing.T) {
	dir := t.TempDir()
	m := Manifest{Source: "abc", Tag: "nebula-20261117-42", Held: []string{}, Packages: map[string]Package{
		"length-gate": lengthGatePackage(),
		"lib":         {Version: "0.1.0", StorePath: "/nix/store/x-lib"},
	}}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, manifestFile, string(data))

	out := filepath.Join(dir, "Formula")
	written, err := writeFormulas(filepath.Join(dir, manifestFile), out)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(out, "length-gate.rb")}
	if !reflect.DeepEqual(written, want) {
		t.Fatalf("written = %v, want %v", written, want)
	}
	got, err := os.ReadFile(want[0])
	if err != nil {
		t.Fatal(err)
	}
	golden, err := os.ReadFile(filepath.Join("testdata", "length-gate.rb"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(golden) {
		t.Fatal("written formula differs from testdata/length-gate.rb")
	}
}

// TestWriteFormulasRemovesFormulaeForToolsNoLongerReleased checks L1: a stale
// Formula/<tool>.rb for a tool the manifest no longer releases is removed.
func TestWriteFormulasRemovesFormulaeForToolsNoLongerReleased(t *testing.T) {
	dir := t.TempDir()
	m := Manifest{Source: "abc", Tag: "nebula-20261117-42", Held: []string{}, Packages: map[string]Package{
		"length-gate": lengthGatePackage(),
	}}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, manifestFile, string(data))
	writeFile(t, dir, "Formula/retired-tool.rb", "class RetiredTool < Formula\nend\n")

	out := filepath.Join(dir, "Formula")
	if _, err := writeFormulas(filepath.Join(dir, manifestFile), out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "retired-tool.rb")); !os.IsNotExist(err) {
		t.Fatalf("retired-tool.rb still present: err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "length-gate.rb")); err != nil {
		t.Fatalf("length-gate.rb missing: %v", err)
	}
}

func TestRenderFormulaRejectsBadInput(t *testing.T) {
	unknown := Package{Version: "1", Systems: map[string]Asset{"riscv64-linux": {URL: "u", SHA256: "s"}}}
	if _, err := renderFormula("length-gate", unknown); !errors.Is(err, errFormulaSystem) {
		t.Fatalf("unknown system: err = %v, want errFormulaSystem", err)
	}
	if _, err := renderFormula("length-gate", Package{Version: "1"}); !errors.Is(err, errFormulaSystem) {
		t.Fatalf("no assets: err = %v, want errFormulaSystem", err)
	}
	if _, err := renderFormula("Length_Gate", lengthGatePackage()); !errors.Is(err, errFormulaName) {
		t.Fatalf("bad name: err = %v, want errFormulaName", err)
	}
}

func TestFormulaClass(t *testing.T) {
	tests := map[string]string{
		"length-gate": "LengthGate",
		"jj-hooks":    "JjHooks",
		"cd":          "Cd",
	}
	for name, want := range tests {
		if got := formulaClass(name); got != want {
			t.Errorf("formulaClass(%q) = %q, want %q", name, got, want)
		}
	}
}
