package transfer

import (
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

	"golang.org/x/sys/unix"
)

const (
	rewriteTempRoot = "opt/data/.nemo-move-tmp"
	fileType        = "file"
	directoryType   = "directory"
	symlinkType     = "symlink"
)

// Manifest records the content and metadata for each transferred destination path.
// Paths use slash separators and are relative to the extraction root.
type Manifest struct {
	Roots   []string        `json:"roots"`
	Entries []ManifestEntry `json:"entries"`
}

// ManifestEntry describes one regular file, directory, or symlink.
type ManifestEntry struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	Mode   uint32 `json:"mode"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
	Target string `json:"target,omitempty"`
}

func scanTree(root string, prefixes []string) (Manifest, error) {
	manifest := Manifest{Roots: append([]string(nil), prefixes...)}
	seen := make(map[string]struct{})
	for _, prefix := range prefixes {
		if err := validateRelativePath(prefix); err != nil {
			return Manifest{}, err
		}
		fullPath := filepath.Join(root, filepath.FromSlash(prefix))
		info, err := os.Lstat(fullPath)
		if err != nil {
			return Manifest{}, fmt.Errorf("stat transfer root %s: %w", prefix, err)
		}
		if !info.IsDir() {
			return Manifest{}, fmt.Errorf("transfer root %s is not a directory", prefix)
		}
		if err := filepath.WalkDir(fullPath, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return fmt.Errorf("walk transfer path %s: %w", path, walkErr)
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return fmt.Errorf("make transfer path relative: %w", err)
			}
			relative = filepath.ToSlash(relative)
			if err := validateRelativePath(relative); err != nil {
				return err
			}
			if _, ok := seen[relative]; ok {
				return nil
			}
			if relative == rewriteTempRoot {
				return filepath.SkipDir
			}
			if strings.HasPrefix(relative, rewriteTempRoot+"/") {
				return nil
			}
			if relative == dataRoot+"/lost+found" {
				info, err := os.Lstat(path)
				if err != nil {
					return fmt.Errorf("stat lost+found: %w", err)
				}
				stat, ok := info.Sys().(*fileStat)
				if !info.IsDir() || !ok || stat.Uid != 0 || stat.Gid != 0 {
					return errors.New("lost+found must be a root-owned directory")
				}
				return filepath.SkipDir
			}
			item, err := manifestEntry(path, relative)
			if err != nil {
				return err
			}
			seen[relative] = struct{}{}
			manifest.Entries = append(manifest.Entries, item)
			return nil
		}); err != nil {
			return Manifest{}, err
		}
	}
	sortManifest(&manifest)
	return manifest, nil
}

func clearRewriteTemp(rootFD int) error {
	parent, _, err := openParent(rootFD, dataRoot+"/placeholder", false)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open data root for rewrite temp cleanup: %w", err)
	}
	removeErr := removeTreeAt(parent, ".nemo-move-tmp")
	return errors.Join(removeErr, unix.Close(parent))
}

func rewriteFilePath(path string) bool {
	return strings.HasPrefix(path, "opt/data/cron/") ||
		path == "opt/data/config.yaml" ||
		path == "opt/data/.env" ||
		strings.HasPrefix(path, "opt/data/scripts/")
}

func manifestEntry(path, relative string) (ManifestEntry, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return ManifestEntry{}, fmt.Errorf("stat %s: %w", relative, err)
	}
	entry := ManifestEntry{Path: relative, Mode: uint32(info.Mode().Perm()), Size: info.Size()}
	switch {
	case info.Mode().IsRegular():
		entry.Type = fileType
		file, err := os.Open(path) //nolint:gosec // Path comes from the transfer tree walk.
		if err != nil {
			return ManifestEntry{}, fmt.Errorf("open %s: %w", relative, err)
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return ManifestEntry{}, fmt.Errorf("hash %s: %w", relative, err)
		}
		entry.SHA256 = hex.EncodeToString(hash.Sum(nil))
	case info.IsDir():
		entry.Type = directoryType
		entry.Size = 0
	case info.Mode()&os.ModeSymlink != 0:
		entry.Type = symlinkType
		entry.Target, err = os.Readlink(path)
		if err != nil {
			return ManifestEntry{}, fmt.Errorf("read symlink %s: %w", relative, err)
		}
	default:
		return ManifestEntry{}, fmt.Errorf("unsupported file type at %s", relative)
	}
	return entry, nil
}

func validateRelativePath(path string) error {
	if path == "" || filepath.IsAbs(path) || strings.ContainsRune(path, '\\') || strings.ContainsRune(path, '\x00') {
		return fmt.Errorf("invalid transfer path %q", path)
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != path {
		return fmt.Errorf("invalid transfer path %q", path)
	}
	return nil
}

func sortManifest(manifest *Manifest) {
	sort.Strings(manifest.Roots)
	sort.Slice(manifest.Entries, func(i, j int) bool {
		return manifest.Entries[i].Path < manifest.Entries[j].Path
	})
}

// WriteManifest atomically replaces path with synced, private manifest data.
func WriteManifest(path string, data []byte) error {
	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, ".nemo-move-manifest-*")
	if err != nil {
		return fmt.Errorf("create temporary manifest: %w", err)
	}
	temporary := file.Name()
	fail := func(cause error) error {
		closeErr := file.Close()
		removeErr := os.Remove(temporary)
		return errors.Join(cause, closeErr, removeErr)
	}
	if err := file.Chmod(0o600); err != nil {
		return fail(fmt.Errorf("set manifest mode: %w", err))
	}
	if _, err := file.Write(data); err != nil {
		return fail(fmt.Errorf("write manifest: %w", err))
	}
	if err := file.Sync(); err != nil {
		return fail(fmt.Errorf("sync manifest: %w", err))
	}
	if err := file.Close(); err != nil {
		removeErr := os.Remove(temporary)
		return errors.Join(fmt.Errorf("close manifest: %w", err), removeErr)
	}
	if err := os.Rename(temporary, path); err != nil {
		removeErr := os.Remove(temporary)
		return errors.Join(fmt.Errorf("rename manifest: %w", err), removeErr)
	}
	directoryFile, err := os.Open(directory) //nolint:gosec // This is the parent of the operator-selected manifest.
	if err != nil {
		return fmt.Errorf("open manifest directory: %w", err)
	}
	syncErr := directoryFile.Sync()
	closeErr := directoryFile.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return fmt.Errorf("sync manifest directory: %w", err)
	}
	return nil
}
