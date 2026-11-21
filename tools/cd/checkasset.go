// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"
)

var errAssetCheck = errors.New("asset build check failed")

// nixStore is the build machine's store. A consumer has no such store, so an
// asset that names a path under it cannot run there.
const nixStore = "/nix/store/"

// checkAsset runs the T6 build check on one built bin/<name> for system. It
// accepts no dynamic linking: see checkAssetWithRuntime for the declared case.
func checkAsset(path, system string) error {
	return checkAssetWithRuntime(path, system, nil)
}

// checkAssetWithRuntime runs the T6 build check. A Linux binary is a static ELF
// for the system's machine, unless the artifact declares its runtime deps: then
// it may carry an interpreter and DT_NEEDED entries, but every DT_NEEDED soname
// must be in runtime, and the interpreter and RPATH/RUNPATH must not name the
// build's /nix/store. A darwin binary is a Mach-O for the system's CPU that
// links only /usr/lib and /System libraries.
func checkAssetWithRuntime(path, system string, runtime []string) error {
	// windows-amd64 names the OS first, so it is routed before the <arch>-<os> split.
	if system == windowsAMD64 {
		return checkPE(path, "amd64")
	}
	arch, osName, ok := strings.Cut(system, "-")
	if !ok {
		return fmt.Errorf("%w: bad system %q", errAssetCheck, system)
	}
	switch osName {
	case "linux":
		return checkELF(path, arch, runtime)
	case "darwin":
		return checkMachO(path, arch)
	}
	return fmt.Errorf("%w: unsupported system %q", errAssetCheck, system)
}

// checkPE requires a Windows PE executable for the system's machine that
// imports only Windows system DLLs. It does not allow a bundled runtime.
func checkPE(path, arch string) error {
	f, err := pe.Open(path)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", errAssetCheck, path, err)
	}
	defer f.Close()
	want := map[string]uint16{"amd64": pe.IMAGE_FILE_MACHINE_AMD64}[arch]
	if want == 0 || f.FileHeader.Machine != want {
		return fmt.Errorf("%w: %s: machine %#x, want %s", errAssetCheck, path, f.FileHeader.Machine, arch)
	}
	if f.FileHeader.Characteristics&pe.IMAGE_FILE_EXECUTABLE_IMAGE == 0 {
		return fmt.Errorf("%w: %s: not an executable image", errAssetCheck, path)
	}
	libs, err := peImportedDLLs(f)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", errAssetCheck, path, err)
	}
	for _, lib := range libs {
		if !strings.HasPrefix(strings.ToLower(lib), "api-ms-") && !strings.HasPrefix(strings.ToLower(lib), "ext-ms-") && !isWindowsSystemDLL(lib) {
			return fmt.Errorf("%w: %s: imports %s, which is not a Windows system DLL", errAssetCheck, path, lib)
		}
	}
	return nil
}

// peImportedDLLs returns the DLL names in the import and delay-import
// directories. debug/pe's ImportedLibraries is a stub that returns nil, so
// the directories are read here. A directory entry is an RVA; the section
// that maps that RVA holds its bytes.
func peImportedDLLs(f *pe.File) ([]string, error) {
	oh, ok := f.OptionalHeader.(*pe.OptionalHeader64)
	if !ok {
		return nil, fmt.Errorf("not a PE32+ image")
	}
	var libs []string
	for _, dirIndex := range []int{pe.IMAGE_DIRECTORY_ENTRY_IMPORT, pe.IMAGE_DIRECTORY_ENTRY_DELAY_IMPORT} {
		dir := oh.DataDirectory[dirIndex]
		if dir.VirtualAddress == 0 {
			continue
		}
		// Import descriptors are 20 bytes; delay descriptors are 32 bytes with
		// the DLL name RVA in the second field. A zero name ends the table.
		desc := 20
		nameOff := 12
		if dirIndex == pe.IMAGE_DIRECTORY_ENTRY_DELAY_IMPORT {
			desc = 32
			nameOff = 4
		}
		for i := uint32(0); ; i++ {
			entry := make([]byte, desc)
			if err := peReadRVA(f, dir.VirtualAddress+i*uint32(desc), entry); err != nil {
				return nil, err
			}
			if allZero(entry) {
				break
			}
			name, err := peCString(f, binary.LittleEndian.Uint32(entry[nameOff:nameOff+4]))
			if err != nil {
				return nil, err
			}
			libs = append(libs, name)
		}
	}
	return libs, nil
}

// peReadRVA fills buf from the section that maps rva.
func peReadRVA(f *pe.File, rva uint32, buf []byte) error {
	for _, s := range f.Sections {
		if rva >= s.VirtualAddress && uint64(rva-s.VirtualAddress)+uint64(len(buf)) <= uint64(s.VirtualSize) {
			_, err := s.ReadAt(buf, int64(rva-s.VirtualAddress))
			return err
		}
	}
	return fmt.Errorf("rva %#x is outside every section", rva)
}

// peCString reads the NUL-terminated string at rva.
func peCString(f *pe.File, rva uint32) (string, error) {
	var out []byte
	buf := make([]byte, 1)
	for len(out) < 256 {
		if err := peReadRVA(f, rva+uint32(len(out)), buf); err != nil {
			return "", err
		}
		if buf[0] == 0 {
			return string(out), nil
		}
		out = append(out, buf[0])
	}
	return "", fmt.Errorf("dll name at rva %#x is longer than 256 bytes", rva)
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// isWindowsSystemDLL reports whether lib is one of the system DLLs every
// Windows install provides.
func isWindowsSystemDLL(lib string) bool {
	switch strings.ToLower(lib) {
	case "kernel32.dll", "ntdll.dll", "user32.dll", "advapi32.dll", "ws2_32.dll",
		"bcrypt.dll", "crypt32.dll", "shell32.dll", "ole32.dll", "msvcrt.dll",
		"ucrtbase.dll", "secur32.dll", "userenv.dll", "iphlpapi.dll":
		return true
	}
	return false
}

func checkELF(path, arch string, runtime []string) error {
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", errAssetCheck, path, err)
	}
	defer f.Close()
	want := map[string]elf.Machine{"x86_64": elf.EM_X86_64, "aarch64": elf.EM_AARCH64}[arch]
	if f.Machine != want {
		return fmt.Errorf("%w: %s: machine %s, want %s", errAssetCheck, path, f.Machine, arch)
	}
	libs, err := f.ImportedLibraries()
	if err != nil {
		return fmt.Errorf("%w: %s: %w", errAssetCheck, path, err)
	}
	if len(runtime) == 0 {
		for _, prog := range f.Progs {
			if prog.Type == elf.PT_INTERP {
				return fmt.Errorf("%w: %s: dynamically linked (has an interpreter)", errAssetCheck, path)
			}
		}
		if len(libs) > 0 {
			return fmt.Errorf("%w: %s: dynamically linked against %q", errAssetCheck, path, libs)
		}
		return nil
	}
	if err := checkInterp(f, arch, path); err != nil {
		return err
	}
	if err := checkRunpath(f, path); err != nil {
		return err
	}
	for _, lib := range libs {
		if !slices.Contains(runtime, lib) {
			return fmt.Errorf("%w: %s: needs %s, which runtimeDeps does not declare", errAssetCheck, path, lib)
		}
	}
	return nil
}

// standardLoader is the dynamic loader a consumer's FHS host provides for each
// Linux machine. The declared branch accepts only this interpreter.
var standardLoader = map[string]string{
	"x86_64":  "/lib64/ld-linux-x86-64.so.2",
	"aarch64": "/lib/ld-linux-aarch64.so.1",
}

// checkInterp requires the declared branch's PT_INTERP to be the system's
// standard loader. Anything else, including a path under the build's /nix/store,
// is refused. A binary with no PT_INTERP is static and passes.
func checkInterp(f *elf.File, arch, path string) error {
	want := standardLoader[arch]
	for _, prog := range f.Progs {
		if prog.Type != elf.PT_INTERP {
			continue
		}
		buf := make([]byte, prog.Filesz)
		if _, err := prog.ReadAt(buf, 0); err != nil {
			return fmt.Errorf("%w: %s: read interpreter: %w", errAssetCheck, path, err)
		}
		interp := strings.TrimRight(string(buf), "\x00")
		if interp != want {
			return fmt.Errorf("%w: %s: interpreter %s, want the standard loader %s", errAssetCheck, path, interp, want)
		}
	}
	return nil
}

// checkRunpath refuses an RPATH or RUNPATH that names the build's /nix/store.
func checkRunpath(f *elf.File, path string) error {
	for _, tag := range []elf.DynTag{elf.DT_RPATH, elf.DT_RUNPATH} {
		paths, err := f.DynString(tag)
		if err != nil {
			return fmt.Errorf("%w: %s: read %v: %w", errAssetCheck, path, tag, err)
		}
		for _, p := range paths {
			if strings.Contains(p, nixStore) {
				return fmt.Errorf("%w: %s: %v names %s", errAssetCheck, path, tag, nixStore)
			}
		}
	}
	return nil
}

func checkMachO(path, arch string) error {
	f, err := macho.Open(path)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", errAssetCheck, path, err)
	}
	defer f.Close()
	want := map[string]macho.Cpu{"x86_64": macho.CpuAmd64, "aarch64": macho.CpuArm64}[arch]
	if f.Cpu != want {
		return fmt.Errorf("%w: %s: cpu %s, want %s", errAssetCheck, path, f.Cpu, arch)
	}
	libs, err := f.ImportedLibraries()
	if err != nil {
		return fmt.Errorf("%w: %s: %w", errAssetCheck, path, err)
	}
	for _, lib := range libs {
		if !strings.HasPrefix(lib, "/usr/lib/") && !strings.HasPrefix(lib, "/System/") {
			return fmt.Errorf("%w: %s: links %s outside /usr/lib and /System", errAssetCheck, path, lib)
		}
	}
	return nil
}
