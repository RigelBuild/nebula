// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"errors"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// windowsCUE declares one binary artifact with the given systems list ("" for none).
func windowsCUE(name, systems string) string {
	field := ""
	if systems != "" {
		field = "\n\t\t\tsystems: [" + systems + "]"
	}
	return `id: "` + name + `"
release: {
	base: "1.2.0"
	artifacts: {
		"` + name + `": {
			kind: "binary"
			binaries: ["` + name + `"]` + field + `
		}
	}
}
images: {}
`
}

// windowsRepo makes a repo with one tool per entry, each component.cue built from windowsCUE.
func windowsRepo(t *testing.T, tools map[string]string) string {
	t.Helper()
	dir := newRepo(t)
	for name, systems := range tools {
		writeFile(t, dir, "tools/"+name+"/component.cue", windowsCUE(name, systems))
		writeFile(t, dir, "tools/"+name+"/go.mod", "module "+name+"\n")
		writeFile(t, dir, "tools/"+name+"/main.go", "package main\n")
	}
	commitAll(t, dir, "windows components")
	return dir
}

// windowsPackage returns the named binary from listBinaries of repo.
func windowsPackage(t *testing.T, repo, name string) Binary {
	t.Helper()
	listed, err := listBinaries(repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range listed {
		if b.Package == name {
			return b
		}
	}
	t.Fatalf("no binary %q in %+v", name, listed)
	return Binary{}
}

// TestWindowsAssetForDeclaringTool: a tool that names windows-amd64 gets a
// Windows asset with the .exe name, and a checksum in the manifest.
func TestWindowsAssetForDeclaringTool(t *testing.T) {
	dir := windowsRepo(t, map[string]string{"gamma": `"x86_64-linux", "windows-amd64"`})
	b := windowsPackage(t, dir, "gamma")
	if !slices.Contains(targetSystems(b, releaseSystems), windowsAMD64) {
		t.Fatalf("targetSystems = %v, want windows-amd64 for a declaring tool", targetSystems(b, releaseSystems))
	}
	want := "gamma-1.2.0-windows-amd64.exe.tar.zst"
	if got := assetName("gamma", "1.2.0", windowsAMD64); got != want {
		t.Fatalf("assetName = %q, want %q", got, want)
	}
	assets := makeAssets(t, []string{"x86_64-linux"}, "gamma")
	writeFile(t, assets, want, "windows bytes")
	m, err := buildManifest(Plan{Source: "s", Build: []string{"gamma"}, Binaries: []Binary{b}}, "nebula-20261117-9", assets, "",
		"https://x", []string{"x86_64-linux", windowsAMD64}, nil)
	if err != nil {
		t.Fatal(err)
	}
	asset, ok := m.Packages["gamma"].Systems[windowsAMD64]
	if !ok {
		t.Fatalf("manifest has no windows-amd64 asset: %+v", m.Packages["gamma"].Systems)
	}
	if !strings.HasSuffix(asset.URL, "/gamma-1.2.0-windows-amd64.exe.tar.zst") || asset.SHA256 == "" {
		t.Fatalf("windows asset = %+v, want the .exe name and a checksum", asset)
	}
}

// TestNonDeclaringToolGetsNoWindowsAsset: a tool with no systems list gets the
// default systems only, and the Windows leg never includes it.
func TestNonDeclaringToolGetsNoWindowsAsset(t *testing.T) {
	dir := windowsRepo(t, map[string]string{"delta": ""})
	b := windowsPackage(t, dir, "delta")
	got := targetSystems(b, releaseSystems)
	if slices.Contains(got, windowsAMD64) {
		t.Fatalf("targetSystems = %v, a non-declaring tool must get no windows-amd64", got)
	}
	if want := defaultSystems; !slices.Equal(got, want) {
		t.Fatalf("targetSystems = %v, want the unchanged default %v", got, want)
	}
}

// TestDefaultAssetSetUnchangedByWindows pins the invariant: adding windows-amd64
// to the release list must not change the asset set of a tool that names no systems.
func TestDefaultAssetSetUnchangedByWindows(t *testing.T) {
	b := Binary{Package: "alpha", Version: "1.2.0"}
	before := targetSystems(b, defaultSystems)
	after := targetSystems(b, releaseSystems)
	if !slices.Equal(before, after) {
		t.Fatalf("asset set changed with windows in the release list: before %v, after %v", before, after)
	}
}

// TestUnknownSystemErrors: an artifact naming a system outside releaseSystems
// fails listBinaries with errUnknownSystem.
func TestUnknownSystemErrors(t *testing.T) {
	dir := windowsRepo(t, map[string]string{"epsilon": `"riscv64-linux"`})
	_, err := listBinaries(dir)
	if !errors.Is(err, errUnknownSystem) {
		t.Fatalf("err = %v, want errUnknownSystem", err)
	}
}

// TestWindowsCheckAssetRefusesNonPE: a Linux ELF is refused for windows-amd64.
func TestWindowsCheckAssetRefusesNonPE(t *testing.T) {
	elfPath := filepath.Join(t.TempDir(), "tool")
	writeFile(t, filepath.Dir(elfPath), "tool", "\x7fELF not a real binary")
	if err := checkAsset(elfPath, windowsAMD64); !errors.Is(err, errAssetCheck) {
		t.Fatalf("err = %v, want errAssetCheck for a non-PE file", err)
	}
}

// TestFormulaSkipsWindowsSystem: a Windows asset is not a Homebrew system, so it
// does not make a formula error, and the formula keeps its macOS block.
func TestFormulaSkipsWindowsSystem(t *testing.T) {
	p := Package{Version: "1.2.0", Systems: map[string]Asset{
		"aarch64-darwin": {URL: testReleaseURL + "gamma-1.2.0-aarch64-darwin.tar.zst", SHA256: sumDarwinArm},
		windowsAMD64:     {URL: testReleaseURL + "gamma-1.2.0-windows-amd64.exe.tar.zst", SHA256: sumLinuxIntel},
	}}
	body, err := renderFormula("gamma", p)
	if err != nil {
		t.Fatalf("renderFormula: %v", err)
	}
	if strings.Contains(string(body), "windows") {
		t.Fatalf("formula names Windows:\n%s", body)
	}
}

// M1 (medium): a project with no planned binaries must not run its test on the
// windows-amd64 leg. The leg filter is the workflow's own jq expression, run
// here so the test cannot drift from cd.yaml.
func TestWindowsLegSkipsProjectWithNoBinaries(t *testing.T) {
	jqPath, err := exec.LookPath("jq")
	if err != nil {
		t.Fatalf("jq is required to run the leg filter: %v", err)
	}
	expr := workflowLegFilter(t, readWorkflow(t))
	plan := `{"binaries":[{"project":"tools/alpha","systems":["x86_64-linux"]}]}`
	cases := []struct{ project, system, want string }{
		{"tools/autoscaler", "windows-amd64", "no"},
		{"tools/autoscaler", "x86_64-linux", "yes"},
		{"tools/alpha", "windows-amd64", "no"},
		{"tools/alpha", "x86_64-linux", "yes"},
	}
	for _, tc := range cases {
		cmd := exec.Command(jqPath, "-r", "--arg", "p", tc.project, "--arg", "s", tc.system, expr)
		cmd.Stdin = strings.NewReader(plan)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("jq %s/%s: %v\n%s", tc.project, tc.system, err, out)
		}
		if got := strings.TrimSpace(string(out)); got != tc.want {
			t.Fatalf("on_leg(%s, %s) = %q, want %q", tc.project, tc.system, got, tc.want)
		}
	}
}

// workflowLegFilter returns the on_leg jq expression from the Test planned
// projects step of cd.yaml.
func workflowLegFilter(t *testing.T, wf string) string {
	t.Helper()
	start := strings.Index(wf, "on_leg=$(jq -r --arg p \"$project\" --arg s \"$SYSTEM\" '")
	if start < 0 {
		t.Fatal("cd.yaml: no on_leg filter in the Test planned projects step")
	}
	body := wf[start+len("on_leg=$(jq -r --arg p \"$project\" --arg s \"$SYSTEM\" '"):]
	end := strings.Index(body, "' \"$RUNNER_TEMP/plan.json\")")
	if end < 0 {
		t.Fatal("cd.yaml: on_leg filter is not closed where expected")
	}
	return body[:end]
}
