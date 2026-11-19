// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// dynELF writes a minimal ELF64 x86_64 executable with a PT_INTERP program
// header naming interp and a SHT_DYNAMIC section whose DT_NEEDED entries are
// needed. It is the smallest shape debug/elf reads as a dynamic binary, so the
// gate sees a real interpreter and real DT_NEEDED entries.
func dynELF(t *testing.T, interp string, needed ...string) string {
	t.Helper()
	le := binary.LittleEndian
	// Layout: ELF header | PT_INTERP phdr | interp | strtab | dynamic | shstrtab | section headers.
	const ehsize, phsize, shsize = 64, 56, 64
	interpStr := interp + "\x00"
	strtab := []byte{0}
	nameOff := make([]uint32, len(needed))
	for i, n := range needed {
		nameOff[i] = uint32(len(strtab))
		strtab = append(strtab, n...)
		strtab = append(strtab, 0)
	}
	// Dynamic entries: DT_NEEDED per name, then DT_NULL.
	dyn := make([]byte, 0, 16*(len(needed)+1))
	for _, off := range nameOff {
		var e [16]byte
		le.PutUint64(e[0:], 1) // DT_NEEDED
		le.PutUint64(e[8:], uint64(off))
		dyn = append(dyn, e[:]...)
	}
	dyn = append(dyn, make([]byte, 16)...) // DT_NULL
	shstr := []byte("\x00.interp\x00.dynstr\x00.dynamic\x00.shstrtab\x00")
	interpOff := uint64(ehsize + phsize)
	strOff := interpOff + uint64(len(interpStr))
	dynOff := strOff + uint64(len(strtab))
	shstrOff := dynOff + uint64(len(dyn))
	shOff := shstrOff + uint64(len(shstr))
	b := make([]byte, shOff+5*shsize)
	copy(b, "\x7fELF")
	b[4], b[5], b[6] = 2, 1, 1
	le.PutUint16(b[16:], 2)      // ET_EXEC
	le.PutUint16(b[18:], 62)     // EM_X86_64
	le.PutUint32(b[20:], 1)      // version
	le.PutUint64(b[32:], ehsize) // phoff
	le.PutUint64(b[40:], shOff)  // shoff
	le.PutUint16(b[52:], ehsize)
	le.PutUint16(b[54:], phsize)
	le.PutUint16(b[56:], 1) // phnum
	le.PutUint16(b[58:], shsize)
	le.PutUint16(b[60:], 5) // shnum
	le.PutUint16(b[62:], 4) // shstrndx
	ph := b[ehsize:]
	le.PutUint32(ph[0:], 3) // PT_INTERP
	le.PutUint64(ph[8:], interpOff)
	le.PutUint64(ph[32:], uint64(len(interpStr)))
	le.PutUint64(ph[40:], uint64(len(interpStr)))
	copy(b[interpOff:], interpStr)
	copy(b[strOff:], strtab)
	copy(b[dynOff:], dyn)
	copy(b[shstrOff:], shstr)
	// Section headers: 0 null, 1 .interp, 2 .dynstr, 3 .dynamic, 4 .shstrtab.
	sh := func(i int, name, typ uint32, off, size uint64, link uint32, entsize uint64) {
		s := b[shOff+uint64(i)*shsize:]
		le.PutUint32(s[0:], name)
		le.PutUint32(s[4:], typ)
		le.PutUint64(s[24:], off)
		le.PutUint64(s[32:], size)
		le.PutUint32(s[40:], link)
		le.PutUint64(s[56:], entsize)
	}
	sh(1, 1, 1, interpOff, uint64(len(interpStr)), 0, 0) // .interp, SHT_PROGBITS
	sh(2, 9, 3, strOff, uint64(len(strtab)), 0, 0)       // .dynstr, SHT_STRTAB
	sh(3, 17, 6, dynOff, uint64(len(dyn)), 2, 16)        // .dynamic, SHT_DYNAMIC, link -> .dynstr
	sh(4, 26, 3, shstrOff, uint64(len(shstr)), 0, 0)     // .shstrtab, SHT_STRTAB
	path := filepath.Join(t.TempDir(), "bin")
	if err := os.WriteFile(path, b, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

const glibcInterp = "/lib64/ld-linux-x86-64.so.2"

// Undeclared dynamic binaries are refused, as before.
func TestCheckAssetRefusesUndeclaredDynamicBinary(t *testing.T) {
	path := dynELF(t, glibcInterp, "libc.so.6")
	if err := checkAsset(path, "x86_64-linux"); !errors.Is(err, errAssetCheck) {
		t.Fatalf("undeclared dynamic binary: err = %v, want errAssetCheck", err)
	}
}

// A declaration that names every NEEDED entry passes.
func TestCheckAssetAcceptsDeclaredRuntimeDeps(t *testing.T) {
	path := dynELF(t, glibcInterp, "libc.so.6", "libm.so.6")
	if err := checkAssetWithRuntime(path, "x86_64-linux", []string{"libc.so.6", "libm.so.6"}); err != nil {
		t.Fatalf("declared runtime deps refused: %v", err)
	}
}

// A NEEDED entry missing from the declaration fails the gate.
func TestCheckAssetRefusesUndeclaredNeededEntry(t *testing.T) {
	path := dynELF(t, glibcInterp, "libc.so.6", "libstdc++.so.6")
	err := checkAssetWithRuntime(path, "x86_64-linux", []string{"libc.so.6"})
	if !errors.Is(err, errAssetCheck) {
		t.Fatalf("extra NEEDED entry: err = %v, want errAssetCheck", err)
	}
}

// A declared dynamic binary whose interpreter lives in /nix/store is refused:
// a consumer has no such store, so the loader would never be found.
func TestCheckAssetRefusesNixStoreInterpreter(t *testing.T) {
	path := dynELF(t, "/nix/store/abc-glibc/lib/ld-linux-x86-64.so.2", "libc.so.6")
	err := checkAssetWithRuntime(path, "x86_64-linux", []string{"libc.so.6"})
	if !errors.Is(err, errAssetCheck) {
		t.Fatalf("nix-store interpreter: err = %v, want errAssetCheck", err)
	}
}

// A static binary passes with no declaration: no interpreter, no NEEDED.
func TestCheckAssetAcceptsStaticBinaryWithoutDeclaration(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "go.mod", "module x\n\ngo 1.27\n")
	writeFile(t, src, "main.go", "package main\n\nfunc main() {}\n")
	out := filepath.Join(src, "static")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64", "GOFLAGS=")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build static: %v\n%s", err, b)
	}
	if err := checkAsset(out, "x86_64-linux"); err != nil {
		t.Fatalf("static binary refused: %v", err)
	}
}
