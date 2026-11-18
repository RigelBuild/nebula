// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// nixCUE declares one nix artifact: attr is the flake attribute, or "" for none.
func nixCUE(name, attr string) string {
	return fmt.Sprintf(`id: %q
release: {
	base: "1.2.0"
	artifacts: {
		%q: {kind: "nix", attr: %q, binaries: [%q]}
	}
}
images: {}
`, name, name, attr, name)
}

// addNixTool writes tools/<name> with a flake.nix and a component.cue for attr.
func addNixTool(t *testing.T, dir, name, attr string) {
	t.Helper()
	writeFile(t, dir, "tools/"+name+"/moon.yml", "layer: tool\n")
	writeFile(t, dir, "tools/"+name+"/flake.nix", "{ outputs = _: { }; }\n")
	writeFile(t, dir, "tools/"+name+"/component.cue", nixCUE(name, attr))
	commitAll(t, dir, "nix tool "+name)
}

func TestPlanPicksTheNixKindWithoutGoMod(t *testing.T) {
	dir := newRepo(t)
	addNixTool(t, dir, "nixtool", "nixtool")
	p, err := makePlan(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	var got *Binary
	for i := range p.Binaries {
		if p.Binaries[i].Package == "nixtool" {
			got = &p.Binaries[i]
		}
	}
	if got == nil {
		t.Fatalf("binaries = %+v, want a nixtool entry", p.Binaries)
	}
	if got.Kind != kindNix || got.Attr != "nixtool" || got.Project != "tools/nixtool" {
		t.Fatalf("nixtool = %+v, want kind %q attr nixtool in tools/nixtool", got, kindNix)
	}
	if !contains(p.Build, "nixtool") {
		t.Fatalf("build = %v, want nixtool built", p.Build)
	}
	if contains(p.NotBuilt, "nixtool") {
		t.Fatalf("notBuilt = %v, a nix artifact has no go.mod and must not be listed", p.NotBuilt)
	}
}

func TestPlanKeepsGoBinariesOnTheGoKind(t *testing.T) {
	dir := newRepo(t)
	addNixTool(t, dir, "nixtool", "nixtool")
	p, err := makePlan(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range p.Binaries {
		if (b.Package == "alpha" || b.Package == "beta") && (b.Kind != kindGo || b.Attr != "") {
			t.Fatalf("%s = %+v, want the go kind with no attr", b.Package, b)
		}
	}
}

func TestPlanRefusesANixArtifactWithoutAttr(t *testing.T) {
	dir := newRepo(t)
	addNixTool(t, dir, "nixtool", "")
	_, err := makePlan(context.Background(), dir)
	if !errors.Is(err, errNixAttr) {
		t.Fatalf("err = %v, want errNixAttr", err)
	}
}

func TestPlanRefusesANixArtifactWithoutFlake(t *testing.T) {
	dir := newRepo(t)
	writeFile(t, dir, "tools/nixtool/moon.yml", "layer: tool\n")
	writeFile(t, dir, "tools/nixtool/component.cue", nixCUE("nixtool", "nixtool"))
	commitAll(t, dir, "nix tool without flake")
	_, err := makePlan(context.Background(), dir)
	if !errors.Is(err, errNixFlake) {
		t.Fatalf("err = %v, want errNixFlake", err)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
