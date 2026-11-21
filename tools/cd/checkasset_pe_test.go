// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"path/filepath"
	"testing"
)

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
