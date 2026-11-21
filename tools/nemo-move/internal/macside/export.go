package macside

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/RigelBuild/nebula/tools/nemo-move/internal/transfer"
)

// Manifest and ManifestEntry use the same format as the importer.
type Manifest = transfer.Manifest
type ManifestEntry = transfer.ManifestEntry

const directoryType = "directory"

// StagingFresh reports whether the newest staged entry is newer than last-quiet.
func StagingFresh(home string) (bool, error) {
	stage := filepath.Join(home, ".local/state/nemo-move-staging")
	info, err := os.Stat(stage)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat staging: %w", err)
	}
	if !info.IsDir() {
		return false, errors.New("staging is not a directory")
	}
	quiet, err := os.Stat(filepath.Join(home, ".local/state/nemo-move/last-quiet"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat last-quiet: %w", err)
	}
	var newest time.Time
	err = filepath.WalkDir(stage, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("walk staging: %w", err)
	}
	return newest.After(quiet.ModTime()), nil
}

// Export writes the home copy roots and page journals after checking freshness.
func Export(home, journalDir string, w io.Writer) (Manifest, error) {
	return export(home, journalDir, w, nil)
}

// ExportChecked streams a second pass and rejects any changed manifest entry.
func ExportChecked(home, journalDir string, w io.Writer, expected Manifest) (Manifest, error) {
	return export(home, journalDir, w, &expected)
}

func export(home, journalDir string, w io.Writer, expected *Manifest) (Manifest, error) {
	fresh, err := StagingFresh(home)
	if err != nil {
		return Manifest{}, err
	}
	if !fresh {
		return Manifest{}, errors.New("staging is not fresh")
	}
	if expected == nil {
		if _, err := Inventory(home, time.Now()); err != nil {
			return Manifest{}, err
		}
	}
	manifest := Manifest{Roots: []string{"opt/data"}, Entries: []ManifestEntry{}}
	seen := map[string]bool{}
	expectedEntries := make(map[string]ManifestEntry)
	if expected != nil {
		for _, entry := range expected.Entries {
			expectedEntries[entry.Path] = entry
		}
	}
	tw := tar.NewWriter(w)
	for _, row := range Rows() {
		if row.Kind != Copy || filepath.IsAbs(row.Path) {
			continue
		}
		if err := addAncestors(tw, row.Dest, &manifest, seen); err != nil {
			return Manifest{}, err
		}
		err := walkRow(home, row, func(path string, info fs.FileInfo) error {
			entry, err := archiveEntry(tw, home, row, path, info)
			if err != nil {
				return err
			}
			if expected != nil && expectedEntries[entry.Path] != entry {
				return fmt.Errorf("source changed during export: %s", entry.Path)
			}
			seen[entry.Path] = true
			manifest.Entries = append(manifest.Entries, entry)
			return nil
		})
		if err != nil {
			return Manifest{}, fmt.Errorf("export %s: %w", row.Path, err)
		}
	}
	if err := exportJournals(tw, journalDir, &manifest); err != nil {
		return Manifest{}, err
	}
	if expected != nil {
		for _, entry := range manifest.Entries {
			if expectedEntries[entry.Path] != entry {
				return Manifest{}, fmt.Errorf("source changed during export: %s", entry.Path)
			}
		}
	}
	sort.Strings(manifest.Roots)
	sort.Slice(manifest.Entries, func(i, j int) bool {
		return manifest.Entries[i].Path < manifest.Entries[j].Path
	})
	if err := tw.Close(); err != nil {
		return Manifest{}, fmt.Errorf("close archive: %w", err)
	}
	return manifest, nil
}

func addAncestors(tw *tar.Writer, dest string, manifest *Manifest, seen map[string]bool) error {
	parent := filepath.ToSlash(filepath.Dir(strings.TrimPrefix(dest, "/")))
	if parent == "." || parent == "opt" || seen[parent] {
		return nil
	}
	if err := addAncestors(tw, parent, manifest, seen); err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: parent, Typeflag: tar.TypeDir, Mode: 0o700}); err != nil {
		return fmt.Errorf("write ancestor %s: %w", parent, err)
	}
	manifest.Entries = append(manifest.Entries, ManifestEntry{Path: parent, Type: directoryType, Mode: 0o700})
	seen[parent] = true
	return nil
}

func exportJournals(tw *tar.Writer, dir string, manifest *Manifest) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read journal directory: %w", err)
	}
	const root = "control/nemo-page"
	if err := tw.WriteHeader(&tar.Header{Name: root, Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		return fmt.Errorf("write journal directory: %w", err)
	}
	manifest.Roots = append(manifest.Roots, root)
	manifest.Entries = append(manifest.Entries, ManifestEntry{Path: root, Type: directoryType, Mode: 0o755})
	for _, item := range entries {
		if !strings.HasSuffix(item.Name(), ".jsonl") {
			continue
		}
		info, err := item.Info()
		if err != nil {
			return fmt.Errorf("stat journal %s: %w", item.Name(), err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("journal %s is not a regular file", item.Name())
		}
		if _, err := checkEntry(dir, filepath.Join(dir, item.Name()), info); err != nil {
			return fmt.Errorf("inventory journal %s: %w", filepath.Join(dir, item.Name()), err)
		}
		row := Row{Path: item.Name(), Dest: root + "/" + item.Name()}
		entry, err := archiveEntry(tw, dir, row, filepath.Join(dir, item.Name()), info)
		if err != nil {
			return fmt.Errorf("export journal %s: %w", item.Name(), err)
		}
		manifest.Entries = append(manifest.Entries, entry)
	}
	return nil
}

func archiveEntry(tw *tar.Writer, home string, row Row, path string, info fs.FileInfo) (ManifestEntry, error) {
	rel, err := filepath.Rel(filepath.Join(home, row.Path), path)
	if err != nil {
		return ManifestEntry{}, err
	}
	name := filepath.ToSlash(filepath.Join(strings.TrimPrefix(row.Dest, "/"), rel))
	var target string
	if info.Mode()&os.ModeSymlink != 0 {
		target, err = os.Readlink(path)
		if err != nil {
			return ManifestEntry{}, err
		}
	}
	header, err := tar.FileInfoHeader(info, target)
	if err != nil {
		return ManifestEntry{}, err
	}
	header.Name = name
	if strings.HasPrefix(name, "control/nemo-page/") {
		header.Mode = 0o600
	}
	if err := tw.WriteHeader(header); err != nil {
		return ManifestEntry{}, err
	}
	entry := ManifestEntry{Path: name, Mode: uint32(header.Mode & 0o777), Target: target}
	switch {
	case info.IsDir():
		entry.Type = directoryType
	case info.Mode()&os.ModeSymlink != 0:
		entry.Type = "symlink"
		entry.Size = info.Size()
	case info.Mode().IsRegular():
		entry.Type = "file"
		entry.Size = info.Size()
		file, err := os.Open(path) //nolint:gosec // Path comes from the home tree walk.
		if err != nil {
			return ManifestEntry{}, err
		}
		hash := sha256.New()
		_, copyErr := io.Copy(io.MultiWriter(tw, hash), file)
		closeErr := file.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return ManifestEntry{}, err
		}
		entry.SHA256 = hex.EncodeToString(hash.Sum(nil))
	default:
		return ManifestEntry{}, fmt.Errorf("unsupported file type at %s", path)
	}
	return entry, nil
}
