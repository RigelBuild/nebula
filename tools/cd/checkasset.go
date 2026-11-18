// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"debug/elf"
	"debug/macho"
	"errors"
	"fmt"
	"strings"
)

var errAssetCheck = errors.New("asset build check failed")

// checkAsset runs the T6 build check on one built bin/<name> for system:
// a Linux binary is a static ELF for the system's machine; a darwin binary is
// a Mach-O for the system's CPU that links only /usr/lib and /System libraries.
func checkAsset(path, system string) error {
	arch, osName, ok := strings.Cut(system, "-")
	if !ok {
		return fmt.Errorf("%w: bad system %q", errAssetCheck, system)
	}
	switch osName {
	case "linux":
		return checkELF(path, arch)
	case "darwin":
		return checkMachO(path, arch)
	}
	return fmt.Errorf("%w: unsupported system %q", errAssetCheck, system)
}

func checkELF(path, arch string) error {
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", errAssetCheck, path, err)
	}
	defer f.Close()
	want := map[string]elf.Machine{"x86_64": elf.EM_X86_64, "aarch64": elf.EM_AARCH64}[arch]
	if f.Machine != want {
		return fmt.Errorf("%w: %s: machine %s, want %s", errAssetCheck, path, f.Machine, arch)
	}
	for _, prog := range f.Progs {
		if prog.Type == elf.PT_INTERP {
			return fmt.Errorf("%w: %s: dynamically linked (has an interpreter)", errAssetCheck, path)
		}
	}
	libs, err := f.ImportedLibraries()
	if err != nil {
		return fmt.Errorf("%w: %s: %w", errAssetCheck, path, err)
	}
	if len(libs) > 0 {
		return fmt.Errorf("%w: %s: dynamically linked against %q", errAssetCheck, path, libs)
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
