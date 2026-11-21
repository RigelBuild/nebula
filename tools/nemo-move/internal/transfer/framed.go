package transfer

import (
	"archive/tar"
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/RigelBuild/nebula/tools/nemo-move/internal/layout"
)

func writeFramedArchive(source io.Reader, output io.Writer) (result error) {
	return writeFramedArchiveTo(source, output, "", nil)
}

func writeFramedArchiveTo(source io.Reader, output io.Writer, spoolDir string, finish func() error) (result error) {
	reader := bufio.NewReader(source)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("read manifest roots: %w", err)
	}
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return fmt.Errorf("decode manifest roots: %w", err)
	}
	for {
		line, err = reader.ReadBytes('\n')
		if err != nil {
			return fmt.Errorf("read manifest entry: %w", err)
		}
		if string(line) == "null\n" {
			break
		}
		var entry ManifestEntry
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&entry); err != nil {
			return fmt.Errorf("decode manifest entry: %w", err)
		}
		if err := validateManifestEntry(entry); err != nil {
			return err
		}
		manifest.Entries = append(manifest.Entries, entry)
	}
	if err := validateManifestRoots(manifest); err != nil {
		return err
	}
	if err := validateManifestPaths(manifest); err != nil {
		return err
	}
	if spoolDir == "" {
		spoolDir = os.TempDir()
	}
	var total uint64
	for _, entry := range manifest.Entries {
		if entry.Type == fileType {
			if entry.Size < 0 || uint64(entry.Size) > uint64(maxInventoryBytes)-total {
				return errors.New("invalid manifest total")
			}
			total += uint64(entry.Size)
		}
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(spoolDir, &stat); err != nil {
		return fmt.Errorf("check spool free space: %w", err)
	}
	if stat.Bsize <= 0 || total > stat.Bavail*uint64(stat.Bsize) {
		return errors.New("insufficient spool free space")
	}
	spool, err := os.CreateTemp(spoolDir, "nemo-move-verified-*")
	if err != nil {
		return fmt.Errorf("create archive spool: %w", err)
	}
	defer func() { result = errors.Join(result, spool.Close(), os.Remove(spool.Name())) }()
	if err := verifyAndSpool(reader, spool, &manifest); err != nil {
		return err
	}
	if finish != nil {
		if _, err := io.Copy(io.Discard, reader); err != nil {
			return fmt.Errorf("finish source archive: %w", err)
		}
		if err := finish(); err != nil {
			return fmt.Errorf("finish source export: %w", err)
		}
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind archive: %w", err)
	}
	return emitArchive(output, data, spool)
}

func verifyAndSpool(source io.Reader, spool io.Writer, manifest *Manifest) error {
	entries := make(map[string]int, len(manifest.Entries))
	for i, entry := range manifest.Entries {
		if _, exists := entries[entry.Path]; exists {
			return fmt.Errorf("duplicate manifest path %s", entry.Path)
		}
		entries[entry.Path] = i
	}
	reader := tar.NewReader(source)
	writer := tar.NewWriter(spool)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read source archive: %w", err)
		}
		index, ok := entries[header.Name]
		if !ok {
			return fmt.Errorf("unexpected archive path %s", header.Name)
		}
		entry := &manifest.Entries[index]
		if err := verifyEntry(reader, writer, header, entry); err != nil {
			return err
		}
		delete(entries, header.Name)
	}
	if len(entries) != 0 {
		return errors.New("archive is missing manifest entries")
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("close archive spool: %w", err)
	}
	return nil
}

func verifyEntry(reader *tar.Reader, writer *tar.Writer, header *tar.Header, entry *ManifestEntry) error {
	kind := ""
	switch header.Typeflag {
	case tar.TypeReg:
		kind = fileType
	case tar.TypeDir:
		kind = directoryType
	case tar.TypeSymlink:
		kind = symlinkType
	}
	if kind == "" || kind != entry.Type || uint32(header.Mode&0o777) != entry.Mode {
		return fmt.Errorf("archive metadata mismatch at %s", header.Name)
	}
	if header.Size < 0 || header.Size > maxInventoryBytes {
		return fmt.Errorf("invalid archive size at %s", header.Name)
	}
	if kind == fileType && header.Size != entry.Size {
		return fmt.Errorf("archive size mismatch at %s", header.Name)
	}
	if kind == symlinkType {
		if header.Linkname != entry.Target || int64(len(header.Linkname)) != entry.Size {
			return fmt.Errorf("archive symlink mismatch at %s", header.Name)
		}
	}
	if err := writer.WriteHeader(header); err != nil {
		return fmt.Errorf("write archive header: %w", err)
	}
	hash := sha256.New()
	if _, err := io.CopyN(io.MultiWriter(writer, hash), reader, header.Size); err != nil {
		return fmt.Errorf("copy archive entry: %w", err)
	}
	if kind == fileType && hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
		return fmt.Errorf("archive digest mismatch at %s", header.Name)
	}
	return nil
}

func rewriteLink(target, sourceHome string, lay layout.Layout) (string, error) {
	if !strings.HasPrefix(target, "/") {
		return target, nil
	}
	target = filepath.Clean(target)
	if target == "/opt/data" || strings.HasPrefix(target, "/opt/data/") {
		return target, nil
	}
	for _, prefix := range homePrefixes(sourceHome, lay) {
		if target == prefix.source || strings.HasPrefix(target, prefix.source+"/") {
			return prefix.dest + strings.TrimPrefix(target, prefix.source), nil
		}
	}
	return "", fmt.Errorf("absolute symlink points outside home: %s", target)
}

func emitArchive(output io.Writer, data []byte, spool io.Reader) error {
	writer := tar.NewWriter(output)
	if err := writer.WriteHeader(&tar.Header{Name: manifestName, Mode: 0o444, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		return fmt.Errorf("write manifest header: %w", err)
	}
	if _, err := writer.Write(data); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	reader := tar.NewReader(spool)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read verified archive: %w", err)
		}
		if err := writer.WriteHeader(header); err != nil {
			return fmt.Errorf("write archive header: %w", err)
		}
		if _, err := io.CopyN(writer, reader, header.Size); err != nil {
			return fmt.Errorf("write archive entry: %w", err)
		}
	}
	return writer.Close()
}
