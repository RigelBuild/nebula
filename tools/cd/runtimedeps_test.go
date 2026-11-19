// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
)

// The plan carries an artifact's runtimeDeps from component.cue to the batch job.
func TestPlanCarriesRuntimeDeps(t *testing.T) {
	dir := newRepo(t)
	writeFile(t, dir, "tools/nixtool/moon.yml", "layer: tool\n")
	writeFile(t, dir, "tools/nixtool/flake.nix", "{ outputs = _: { }; }\n")
	writeFile(t, dir, "tools/nixtool/component.cue", `id: "nixtool"
release: {
	base: "1.2.0"
	artifacts: {
		"nixtool": {kind: "nix", attr: "nixtool", binaries: ["nixtool"], runtimeDeps: ["libc.so.6", "libm.so.6"]}
	}
}
images: {}
`)
	commitAll(t, dir, "nix tool with runtime deps")
	p, err := makePlan(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range p.Binaries {
		if b.Package == "nixtool" {
			if want := []string{"libc.so.6", "libm.so.6"}; !reflect.DeepEqual(b.RuntimeDeps, want) {
				t.Fatalf("runtimeDeps = %v, want %v", b.RuntimeDeps, want)
			}
			return
		}
	}
	t.Fatal("plan has no nixtool binary")
}

// cd check-asset takes -runtime, so cd.yaml can pass the declared sonames.
func TestCheckAssetCLIPassesRuntimeFlag(t *testing.T) {
	path := dynELF(t, glibcInterp, "libc.so.6")
	var out, errb bytes.Buffer
	ctx := context.Background()
	if err := dispatch(ctx, []string{"check-asset", "-system", "x86_64-linux", "-runtime", "libc.so.6", path}, &out, &errb); err != nil {
		t.Fatalf("declared -runtime refused: %v", err)
	}
	if err := dispatch(ctx, []string{"check-asset", "-system", "x86_64-linux", path}, &out, &errb); !errors.Is(err, errAssetCheck) {
		t.Fatalf("no -runtime: err = %v, want errAssetCheck", err)
	}
}
