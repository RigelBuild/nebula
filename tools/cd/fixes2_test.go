// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// L2: a package with no darwin asset gets no Homebrew formula, because the tap
// needs an on_macos block. The Release itself still ships its Linux assets.
func TestWriteFormulasSkipsPackageWithoutDarwinAsset(t *testing.T) {
	dir := t.TempDir()
	linuxOnly := Package{Version: "0.1.0", Systems: map[string]Asset{
		"x86_64-linux": {URL: testReleaseURL + "trunk-mq-mcp-0.1.0-x86_64-linux.tar.zst", SHA256: sumDarwinArm},
	}}
	m := Manifest{Source: "abc", Tag: "nebula-20261117-43", Held: []string{}, Packages: map[string]Package{
		"length-gate":   lengthGatePackage(),
		"trunk-mq-mcp":  linuxOnly,
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
		t.Fatalf("written = %v, want only length-gate (no trunk-mq-mcp, which has no darwin asset)", written)
	}
	if _, err := os.Stat(filepath.Join(out, "trunk-mq-mcp.rb")); !os.IsNotExist(err) {
		t.Fatalf("trunk-mq-mcp.rb exists for a package with no darwin asset: err = %v", err)
	}
}

// L1 (workflow): the test step skips a project whose artifacts exclude the leg.
// The gate is a jq expression over the planned binaries, so the workflow must
// carry it and the matrix system must reach it.
func TestTestStepGatesProjectsBySystems(t *testing.T) {
	wf := readWorkflow(t)
	if !strings.Contains(wf, `index($s)`) && !strings.Contains(wf, `index($system)`) {
		t.Fatal("cd.yaml: the Test planned projects step has no systems gate")
	}
	testStart := strings.Index(wf, "- name: Test planned projects")
	buildStart := strings.Index(wf, "- name: Build and check binaries")
	if testStart < 0 || buildStart < testStart {
		t.Fatal("cd.yaml: could not locate the Test planned projects step")
	}
	step := wf[testStart:buildStart]
	if !strings.Contains(step, "systems") {
		t.Fatal("cd.yaml: Test planned projects does not read the artifact systems")
	}
}

// readWorkflow returns .github/workflows/cd.yaml from the repository root.
func readWorkflow(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "cd.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
