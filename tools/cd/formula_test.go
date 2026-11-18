// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
