// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// goMainCUE declares one go artifact in tools/<name>, with main as its build path.
func goMainCUE(name, main string) string {
	return fmt.Sprintf(`id: %q
release: {
	base: "1.2.0"
	artifacts: {
		%q: {kind: "binary", package: "local:%s", binaries: [%q], main: %q}
	}
}
images: {}
`, name, name, name, name, main)
}

// TestPlanGoMainDefaultsToProjectRoot pins the default: a Go artifact that omits
// main builds the project root, as cd.yaml did before the field existed.
func TestPlanGoMainDefaultsToProjectRoot(t *testing.T) {
	dir := newRepo(t)
	p, err := makePlan(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range p.Binaries {
		if b.Package == "alpha" && b.Main != "." {
			t.Fatalf("alpha main = %q, want \".\"", b.Main)
		}
	}
}

// TestPlanCarriesDeclaredGoMain pins the field: a declared main reaches the plan,
// and the Go artifact is built from that package, not the project root.
func TestPlanCarriesDeclaredGoMain(t *testing.T) {
	dir := newRepo(t)
	writeFile(t, dir, "tools/alpha/cmd/alpha/main.go", "package main\n")
	writeFile(t, dir, "tools/alpha/component.cue", goMainCUE("alpha", "./cmd/alpha"))
	commitAll(t, dir, "alpha main under cmd")
	p, err := makePlan(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range p.Binaries {
		if b.Package == "alpha" {
			if b.Main != "./cmd/alpha" {
				t.Fatalf("alpha main = %q, want ./cmd/alpha", b.Main)
			}
			if !contains(p.Build, "alpha") {
				t.Fatalf("build = %v, want alpha built", p.Build)
			}
			return
		}
	}
	t.Fatal("plan has no alpha binary")
}

// TestPlanRefusesMainOutsideProject: main is a path inside the project, so a
// build cannot point at another tree or an absolute path.
func TestPlanRefusesMainOutsideProject(t *testing.T) {
	dir := newRepo(t)
	writeFile(t, dir, "tools/alpha/component.cue", goMainCUE("alpha", "../beta"))
	commitAll(t, dir, "alpha main outside project")
	_, err := makePlan(t.Context(), dir)
	if !errors.Is(err, errMainPath) {
		t.Fatalf("err = %v, want errMainPath", err)
	}
}

// TestPlanRefusesMainOnNixKind: a nix artifact builds from its flake attr, so a
// main path would be ignored. The plan refuses it rather than ignore it.
func TestPlanRefusesMainOnNixKind(t *testing.T) {
	dir := newRepo(t)
	writeFile(t, dir, "tools/nixtool/moon.yml", "layer: tool\n")
	writeFile(t, dir, "tools/nixtool/flake.nix", "{ outputs = _: { }; }\n")
	writeFile(t, dir, "tools/nixtool/component.cue", `id: "nixtool"
release: {
	base: "1.2.0"
	artifacts: {
		"nixtool": {kind: "nix", attr: "nixtool", binaries: ["nixtool"], main: "./cmd/nixtool"}
	}
}
images: {}
`)
	commitAll(t, dir, "nix tool with main")
	_, err := makePlan(t.Context(), dir)
	if !errors.Is(err, errMainNotGo) {
		t.Fatalf("err = %v, want errMainNotGo", err)
	}
}

// TestMainPackageResolvesInsideProject pins the normalization that the plan and
// the build share: "" and "." mean the root, and other paths are cleaned.
func TestMainPackageResolvesInsideProject(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "."},
		{".", "."},
		{"./", "."},
		{"cmd/x", "./cmd/x"},
		{"./cmd/x/", "./cmd/x"},
		{"cmd/./x", "./cmd/x"},
	}
	for _, c := range cases {
		got, err := mainPackage(c.in)
		if err != nil {
			t.Fatalf("mainPackage(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("mainPackage(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	for _, in := range []string{"..", "../x", "/abs", "cmd/../../x"} {
		if _, err := mainPackage(in); !errors.Is(err, errMainPath) {
			t.Fatalf("mainPackage(%q): err = %v, want errMainPath", in, err)
		}
	}
}

// TestRealCmdToolsDeclareTheirMainPackage checks that every Go binary CD builds has a main.go
// at its planned main package. A cmd/ tool with no declared main fails here.
func TestRealCmdToolsDeclareTheirMainPackage(t *testing.T) {
	repo := filepath.Join("..", "..")
	binaries, err := listBinaries(repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, binary := range binaries {
		if binary.Kind != kindGo {
			continue
		}
		if _, err := os.Stat(filepath.Join(repo, binary.Project, "go.mod")); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			t.Fatalf("%s: stat go.mod: %v", binary.Package, err)
		}
		if _, err := os.Stat(filepath.Join(repo, binary.Project, binary.Main, "main.go")); err != nil {
			t.Errorf("%s: planned main %q has no main.go: %v", binary.Package, binary.Main, err)
		}
	}
}

// TestBuildStepBuildsGoFromPlannedMain: the Go branch of the build step reads
// main from the plan and passes it to go build, so a cmd/ tool builds.
func TestBuildStepBuildsGoFromPlannedMain(t *testing.T) {
	wf := readWorkflow(t)
	if !strings.Contains(wf, `main=$(jq -r .main <<<"$b")`) {
		t.Fatal("cd.yaml: the build step does not read main from the plan")
	}
	if !strings.Contains(wf, `go -C "$project" build -trimpath -o "$stage/bin/$name$exe" "$main"`) {
		t.Fatal("cd.yaml: go build does not build the planned main package")
	}
	if strings.Contains(wf, `go -C "$project" build -trimpath -o "$stage/bin/$name$exe" .`) {
		t.Fatal("cd.yaml: go build still hardcodes the project root")
	}
}

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
// three default release systems, not windows-amd64.
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

// L1: a nix artifact that names windows-amd64 fails listBinaries with
// errNixWindows. A nix build has no Windows target.
func TestNixArtifactRefusesWindows(t *testing.T) {
	dir := newRepo(t)
	writeFile(t, dir, "tools/zeta/component.cue", `id: "zeta"
release: {
	base: "1.2.0"
	artifacts: {
		"zeta": {kind: "nix", attr: "zeta", binaries: ["zeta"], systems: ["windows-amd64"]}
	}
}
images: {}
`)
	writeFile(t, dir, "tools/zeta/flake.nix", "{ outputs = _: { }; }\n")
	writeFile(t, dir, "tools/zeta/go.mod", "module zeta\n")
	commitAll(t, dir, "nix windows component")
	_, err := listBinaries(dir)
	if !errors.Is(err, errNixWindows) {
		t.Fatalf("err = %v, want errNixWindows", err)
	}
}
