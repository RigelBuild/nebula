// SPDX-License-Identifier: MIT OR Apache-2.0

package waveseats

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"syscall"
)

const (
	PolicyTablePath  = "/var/lib/cotal-wave/policy-table.json"
	EntitlementsPath = "/var/lib/cotal-wave/entitlements.json"
)

type TrustedTable struct {
	Text   string
	Digest string
}

// ReadTrustedTable reads a pinned table through an owner- and mode-checked descriptor.
func ReadTrustedTable(path, listPath string) (TrustedTable, error) {
	if path != PolicyTablePath && path != EntitlementsPath {
		return TrustedTable{}, fmt.Errorf("permission-denied: untrusted table path %s", path)
	}
	listInfo, err := os.Stat(listPath)
	if err != nil {
		return TrustedTable{}, fmt.Errorf("stat seat list: %w", err)
	}
	owner, ok := fileOwner(listInfo)
	if !ok {
		return TrustedTable{}, errors.New("permission-denied: cannot determine seat list owner")
	}
	return readTrustedDescriptor(path, owner)
}

func readTrustedDescriptor(path string, owner uint32) (result TrustedTable, returnErr error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return TrustedTable{}, fmt.Errorf("open trusted table %s: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer func() {
		returnErr = errors.Join(returnErr, file.Close())
	}()
	info, err := file.Stat()
	if err != nil {
		return TrustedTable{}, fmt.Errorf("stat trusted table descriptor: %w", err)
	}
	tableOwner, ok := fileOwner(info)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 || tableOwner != owner {
		return TrustedTable{}, fmt.Errorf("permission-denied: untrusted table %s", path)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return TrustedTable{}, fmt.Errorf("read trusted table %s: %w", path, err)
	}
	text := string(data)
	return TrustedTable{Text: text, Digest: DigestText(text)}, nil
}

func fileOwner(info fs.FileInfo) (uint32, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return stat.Uid, true
}
