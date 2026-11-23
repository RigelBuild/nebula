// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	if !strings.Contains(wf, `go -C "$project" build -trimpath -o "$stage/bin/$name" "$main"`) {
		t.Fatal("cd.yaml: go build does not build the planned main package")
	}
	if strings.Contains(wf, `go -C "$project" build -trimpath -o "$stage/bin/$name" .`) {
		t.Fatal("cd.yaml: go build still hardcodes the project root")
	}
}
