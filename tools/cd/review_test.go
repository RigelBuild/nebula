// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// dynELFWithRunpath writes a dynamic x86_64 ELF with a PT_INTERP of interp,
// DT_NEEDED entries, and one DT_RPATH or DT_RUNPATH (tag) naming path.
func dynELFWithRunpath(t *testing.T, tag uint64, path string, interp string, needed ...string) string {
	t.Helper()
	le := binary.LittleEndian
	const ehsize, phsize, shsize = 64, 56, 64
	interpStr := interp + "\x00"
	strtab := []byte{0}
	add := func(s string) uint32 {
		off := uint32(len(strtab))
		strtab = append(strtab, s...)
		strtab = append(strtab, 0)
		return off
	}
	var dyn []byte
	put := func(tg, val uint64) {
		var e [16]byte
		le.PutUint64(e[0:], tg)
		le.PutUint64(e[8:], val)
		dyn = append(dyn, e[:]...)
	}
	for _, n := range needed {
		put(1, uint64(add(n))) // DT_NEEDED
	}
	put(tag, uint64(add(path)))
	put(0, 0) // DT_NULL
	shstr := []byte("\x00.interp\x00.dynstr\x00.dynamic\x00.shstrtab\x00")
	interpOff := uint64(ehsize + phsize)
	strOff := interpOff + uint64(len(interpStr))
	dynOff := strOff + uint64(len(strtab))
	shstrOff := dynOff + uint64(len(dyn))
	shOff := shstrOff + uint64(len(shstr))
	b := make([]byte, shOff+5*shsize)
	copy(b, "\x7fELF")
	b[4], b[5], b[6] = 2, 1, 1
	le.PutUint16(b[16:], 2)
	le.PutUint16(b[18:], 62)
	le.PutUint32(b[20:], 1)
	le.PutUint64(b[32:], ehsize)
	le.PutUint64(b[40:], shOff)
	le.PutUint16(b[52:], ehsize)
	le.PutUint16(b[54:], phsize)
	le.PutUint16(b[56:], 1)
	le.PutUint16(b[58:], shsize)
	le.PutUint16(b[60:], 5)
	le.PutUint16(b[62:], 4)
	ph := b[ehsize:]
	le.PutUint32(ph[0:], 3)
	le.PutUint64(ph[8:], interpOff)
	le.PutUint64(ph[32:], uint64(len(interpStr)))
	le.PutUint64(ph[40:], uint64(len(interpStr)))
	copy(b[interpOff:], interpStr)
	copy(b[strOff:], strtab)
	copy(b[dynOff:], dyn)
	copy(b[shstrOff:], shstr)
	sh := func(i int, name, typ uint32, off, size uint64, link uint32, entsize uint64) {
		s := b[shOff+uint64(i)*shsize:]
		le.PutUint32(s[0:], name)
		le.PutUint32(s[4:], typ)
		le.PutUint64(s[24:], off)
		le.PutUint64(s[32:], size)
		le.PutUint32(s[40:], link)
		le.PutUint64(s[56:], entsize)
	}
	sh(1, 1, 1, interpOff, uint64(len(interpStr)), 0, 0)
	sh(2, 9, 3, strOff, uint64(len(strtab)), 0, 0)
	sh(3, 17, 6, dynOff, uint64(len(dyn)), 2, 16)
	sh(4, 26, 3, shstrOff, uint64(len(shstr)), 0, 0)
	out := filepath.Join(t.TempDir(), "bin")
	if err := os.WriteFile(out, b, 0o755); err != nil {
		t.Fatal(err)
	}
	return out
}

const dtRPATH, dtRUNPATH = 15, 29

// H-M1: a /nix/store RUNPATH is refused in the declared branch.
func TestCheckAssetRefusesNixStoreRunpath(t *testing.T) {
	path := dynELFWithRunpath(t, dtRUNPATH, "/nix/store/abc-lib/lib", glibcInterp, "libc.so.6")
	err := checkAssetWithRuntime(path, "x86_64-linux", []string{"libc.so.6"})
	if !errors.Is(err, errAssetCheck) {
		t.Fatalf("/nix/store RUNPATH: err = %v, want errAssetCheck", err)
	}
}

// H-M1: a /nix/store RPATH is refused in the declared branch.
func TestCheckAssetRefusesNixStoreRpath(t *testing.T) {
	path := dynELFWithRunpath(t, dtRPATH, "/nix/store/abc-lib/lib", glibcInterp, "libc.so.6")
	err := checkAssetWithRuntime(path, "x86_64-linux", []string{"libc.so.6"})
	if !errors.Is(err, errAssetCheck) {
		t.Fatalf("/nix/store RPATH: err = %v, want errAssetCheck", err)
	}
}

// M1: a soname that only prefixes a declared one is not the same library.
func TestCheckAssetRefusesSonamePrefix(t *testing.T) {
	path := dynELF(t, glibcInterp, "libc.so.6")
	err := checkAssetWithRuntime(path, "x86_64-linux", []string{"libc.so"})
	if !errors.Is(err, errAssetCheck) {
		t.Fatalf("-runtime libc.so for NEEDED libc.so.6: err = %v, want errAssetCheck", err)
	}
}

// M2: the declared branch refuses an interpreter that is not the system's loader.
func TestCheckAssetRefusesNonStandardInterp(t *testing.T) {
	path := dynELF(t, "/usr/lib/other-ld.so", "libc.so.6")
	err := checkAssetWithRuntime(path, "x86_64-linux", []string{"libc.so.6"})
	if !errors.Is(err, errAssetCheck) {
		t.Fatalf("non-standard interp: err = %v, want errAssetCheck", err)
	}
}

// M2: the standard loader for the system is accepted.
func TestCheckAssetAcceptsStandardInterp(t *testing.T) {
	path := dynELF(t, glibcInterp, "libc.so.6")
	if err := checkAssetWithRuntime(path, "x86_64-linux", []string{"libc.so.6"}); err != nil {
		t.Fatalf("standard loader refused: %v", err)
	}
}

// L1: runtimeDeps on a non-nix artifact is refused at plan time.
func TestPlanRefusesRuntimeDepsOnGoKind(t *testing.T) {
	dir := newRepo(t)
	writeFile(t, dir, "tools/gotool/moon.yml", "layer: tool\n")
	writeFile(t, dir, "tools/gotool/go.mod", "module gotool\n")
	writeFile(t, dir, "tools/gotool/main.go", "package main\n")
	writeFile(t, dir, "tools/gotool/component.cue", `id: "gotool"
release: {
	base: "1.0.0"
	artifacts: {
		"gotool": {kind: "binary", package: "local:gotool", binaries: ["gotool"], runtimeDeps: ["libc.so.6"]}
	}
}
images: {}
`)
	commitAll(t, dir, "go tool with runtime deps")
	_, err := makePlan(context.Background(), dir)
	if !errors.Is(err, errRuntimeNotNix) {
		t.Fatalf("runtimeDeps on a go artifact: err = %v, want errRuntimeNotNix", err)
	}
}

// H1/H2: a systems field limits an artifact, and the plan records it.
func TestPlanCarriesSystemsAndSkipsOthers(t *testing.T) {
	dir := newRepo(t)
	writeFile(t, dir, "tools/nixtool/moon.yml", "layer: tool\n")
	writeFile(t, dir, "tools/nixtool/flake.nix", "{ outputs = _: { }; }\n")
	writeFile(t, dir, "tools/nixtool/component.cue", `id: "nixtool"
release: {
	base: "1.2.0"
	artifacts: {
		"nixtool": {kind: "nix", attr: "nixtool", binaries: ["nixtool"], systems: ["x86_64-linux"]}
	}
}
images: {}
`)
	commitAll(t, dir, "nix tool limited to x86_64-linux")
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
		t.Fatal("plan has no nixtool binary")
	}
	if want := []string{"x86_64-linux"}; !reflect.DeepEqual(got.Systems, want) {
		t.Fatalf("systems = %v, want %v", got.Systems, want)
	}
	if contains(p.NotBuilt, "nixtool") {
		t.Fatalf("notBuilt = %v: a system limit is not a missing build", p.NotBuilt)
	}
}

// H1/H2: a system-limited artifact is listed only for the systems it builds,
// so the Release does not need an asset for a system the artifact skips.
func TestManifestListsOnlyTheSystemsAnArtifactTargets(t *testing.T) {
	p := Plan{Source: "src", Binaries: []Binary{{Package: "nixtool", Version: "1.2.0", Systems: []string{"x86_64-linux"}}}, Build: []string{"nixtool"}}
	assets := makeAssets(t, []string{"x86_64-linux"}, "nixtool")
	m, err := buildManifest(p, "nebula-20261117-3", assets, "", "https://x", []string{"x86_64-linux", "aarch64-linux"}, nil)
	if err != nil {
		t.Fatalf("system-limited artifact failed the release: %v", err)
	}
	got := m.Packages["nixtool"].Systems
	if _, ok := got["aarch64-linux"]; ok {
		t.Fatalf("manifest lists aarch64-linux for an x86_64-only artifact: %v", got)
	}
	if _, ok := got["x86_64-linux"]; !ok {
		t.Fatalf("manifest lacks its own system: %v", got)
	}
}
