// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"bytes"
	"encoding/binary"
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

// fixturePE builds a minimal PE32+ AMD64 executable whose import directory
// names the given DLLs. The .idata section sits at RVA 0x1000 and holds one
// import descriptor per DLL, a null descriptor, then the DLL names.
func fixturePE(t *testing.T, dlls ...string) []byte {
	t.Helper()
	const sectRVA = 0x1000
	const sectSize = 0x200
	const fileAlign = 0x200
	// Layout inside the section: descriptors (20 bytes each, plus a null
	// terminator), then the NUL-terminated names.
	descStart := uint32(0)
	namesStart := uint32(20 * (len(dlls) + 1))
	var section bytes.Buffer
	section.Write(make([]byte, namesStart))
	nameRVAs := make([]uint32, len(dlls))
	for i, dll := range dlls {
		nameRVAs[i] = sectRVA + uint32(section.Len())
		section.WriteString(dll)
		section.WriteByte(0)
	}
	for i := range dlls {
		desc := make([]byte, 20)
		binary.LittleEndian.PutUint32(desc[12:16], nameRVAs[i]) // Name RVA
		copy(section.Bytes()[descStart+uint32(i*20):], desc)
	}
	body := section.Bytes()
	if len(body) > sectSize {
		t.Fatalf("fixture section %d bytes overflows %d", len(body), sectSize)
	}

	var out bytes.Buffer
	// DOS header: e_magic and e_lfanew at offset 0x3c.
	dos := make([]byte, 0x40)
	copy(dos, "MZ")
	binary.LittleEndian.PutUint32(dos[0x3c:], 0x40)
	out.Write(dos)
	out.WriteString("PE\x00\x00")
	// COFF file header.
	coff := make([]byte, 20)
	binary.LittleEndian.PutUint16(coff[0:2], 0x8664) // IMAGE_FILE_MACHINE_AMD64
	binary.LittleEndian.PutUint16(coff[2:4], 1)      // one section
	binary.LittleEndian.PutUint16(coff[16:18], 240)  // SizeOfOptionalHeader for PE32+
	binary.LittleEndian.PutUint16(coff[18:20], 0x22) // EXECUTABLE_IMAGE | LARGE_ADDRESS_AWARE
	out.Write(coff)
	// PE32+ optional header (240 bytes) with 16 data directories.
	opt := make([]byte, 240)
	binary.LittleEndian.PutUint16(opt[0:2], 0x20b)              // PE32+
	binary.LittleEndian.PutUint32(opt[4:8], sectSize)           // SizeOfCode
	binary.LittleEndian.PutUint32(opt[32:36], 0x1000)           // SectionAlignment
	binary.LittleEndian.PutUint32(opt[36:40], fileAlign)        // FileAlignment
	binary.LittleEndian.PutUint32(opt[56:60], sectRVA+sectSize) // SizeOfImage
	binary.LittleEndian.PutUint32(opt[60:64], 0x400)            // SizeOfHeaders
	binary.LittleEndian.PutUint32(opt[108:112], 16)             // NumberOfRvaAndSizes
	// Data directory 1 (import): VirtualAddress, Size. Offset 112 + 8*1.
	binary.LittleEndian.PutUint32(opt[120:124], sectRVA+descStart)
	binary.LittleEndian.PutUint32(opt[124:128], uint32(20*(len(dlls)+1)))
	out.Write(opt)
	// Section header ".idata".
	sect := make([]byte, 40)
	copy(sect[0:8], ".idata")
	binary.LittleEndian.PutUint32(sect[8:12], sectSize)    // VirtualSize
	binary.LittleEndian.PutUint32(sect[12:16], sectRVA)    // VirtualAddress
	binary.LittleEndian.PutUint32(sect[16:20], sectSize)   // SizeOfRawData
	binary.LittleEndian.PutUint32(sect[20:24], 0x400)      // PointerToRawData
	binary.LittleEndian.PutUint32(sect[36:40], 0x40000040) // READ | INITIALIZED_DATA
	out.Write(sect)
	// Pad to the raw data offset and write the section body.
	for out.Len() < 0x400 {
		out.WriteByte(0)
	}
	padded := make([]byte, sectSize)
	copy(padded, body)
	out.Write(padded)
	return out.Bytes()
}

// F3 (medium): the allow-list must reject a non-system DLL and accept system
// DLLs. debug/pe ImportedLibraries returns nil, so this test covers the parser.
func TestWindowsCheckAssetRefusesNonSystemDLL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool.exe")
	writeFile(t, filepath.Dir(path), "tool.exe", string(fixturePE(t, "KERNEL32.dll", "vendorcrypto.dll")))
	err := checkAsset(path, windowsAMD64)
	if !errors.Is(err, errAssetCheck) {
		t.Fatalf("err = %v, want errAssetCheck for a non-system DLL", err)
	}
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("vendorcrypto.dll")) {
		t.Fatalf("err = %v, want it to name the non-system DLL", err)
	}
}

func TestWindowsCheckAssetAcceptsSystemDLLs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool.exe")
	writeFile(t, filepath.Dir(path), "tool.exe", string(fixturePE(t, "KERNEL32.dll", "ws2_32.dll")))
	if err := checkAsset(path, windowsAMD64); err != nil {
		t.Fatalf("a PE importing only system DLLs was refused: %v", err)
	}
}

// F1 (high): the Windows leg must not evaluate a devShell the flake does not
// define, and an empty systems list must not match the Windows leg.
func TestWindowsLegUsesLinuxDevShellAndExplicitFilter(t *testing.T) {
	wf := readWorkflow(t)
	if !strings.Contains(wf, "shell_system=x86_64-linux") {
		t.Fatal("cd.yaml: windows-amd64 does not map to the x86_64-linux devShell; the flake defines no windows devShell")
	}
	if !strings.Contains(wf, `$s != "windows-amd64"`) {
		t.Fatal("cd.yaml: an empty systems list still matches the windows-amd64 leg")
	}
}

// F2 (high): the release job must publish windows-amd64 assets to the Release.
func TestPublishStepIncludesWindowsSystem(t *testing.T) {
	wf := readWorkflow(t)
	start := strings.Index(wf, "- name: Publish release-manifest.json on main")
	end := strings.Index(wf, "- name: Tag the release commit")
	if start < 0 || end < start {
		t.Fatal("cd.yaml: could not locate the publish step")
	}
	if !strings.Contains(wf[start:end], "-system windows-amd64") {
		t.Fatal("cd.yaml: the publish step does not pass -system windows-amd64, so Windows assets never reach the Release")
	}
}
