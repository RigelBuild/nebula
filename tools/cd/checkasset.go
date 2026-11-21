// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"debug/elf"
	"debug/macho"
	"debug/pe"
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
	arch, osName, ok := strings.Cut(system, "-")
	if !ok {
		return fmt.Errorf("%w: bad system %q", errAssetCheck, system)
	}
	switch osName {
	case "linux":
		return checkELF(path, arch, runtime)
	case "darwin":
		return checkMachO(path, arch)
	case "windows":
		return checkPE(path, arch)
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
	libs, err := f.ImportedLibraries()
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
