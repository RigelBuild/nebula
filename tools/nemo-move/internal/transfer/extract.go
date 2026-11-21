package transfer

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Extract imports a manifest-first archive without trusting archive ownership.
func Extract(source io.Reader, root string, opts Options) (Manifest, error) {
	compare := func(root string, want Manifest) error { return compareManifest(root, want, opts.Layout) }
	return extract(source, root, opts, applyOwnership, compare)
}

func extract(source io.Reader, root string, opts Options, ownership func(string, Manifest) error, compare func(string, Manifest) error) (result Manifest, resultErr error) {
	if err := opts.validateSourceHome(); err != nil {
		return Manifest{}, err
	}
	archive := tar.NewReader(source)
	header, err := archive.Next()
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest header: %w", err)
	}
	if header.Name != manifestName || header.Typeflag != tar.TypeReg || header.Size < 0 {
		return Manifest{}, errors.New("archive does not begin with a valid manifest")
	}
	data, err := io.ReadAll(io.LimitReader(archive, header.Size+1))
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest: %w", err)
	}
	if int64(len(data)) != header.Size {
		return Manifest{}, errors.New("manifest size does not match tar header")
	}
	manifest, err := decodeManifest(data)
	if err != nil {
		return Manifest{}, err
	}
	if err := validateManifestRoots(manifest); err != nil {
		return Manifest{}, err
	}
	if err := validateManifestPaths(manifest); err != nil {
		return Manifest{}, err
	}
	remaining := make(map[string]ManifestEntry, len(manifest.Entries))
	for _, entry := range manifest.Entries {
		if _, exists := remaining[entry.Path]; exists {
			return Manifest{}, fmt.Errorf("duplicate manifest entry %s", entry.Path)
		}
		remaining[entry.Path] = entry
	}
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return Manifest{}, fmt.Errorf("open transfer root: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, unix.Close(rootFD)) }()
	if err := clearDataRoots(rootFD, manifest); err != nil {
		return Manifest{}, err
	}
	if err := clearRewriteTemp(rootFD); err != nil {
		return Manifest{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, clearRewriteTemp(rootFD)) }()
	if err := clearIncomingJournals(rootFD, manifest); err != nil {
		return Manifest{}, err
	}
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Manifest{}, fmt.Errorf("read transfer tar header: %w", err)
		}
		if err := validateRelativePath(header.Name); err != nil {
			return Manifest{}, err
		}
		entry, ok := remaining[header.Name]
		if !ok {
			return Manifest{}, fmt.Errorf("unexpected or duplicate archive path %s", header.Name)
		}
		if err := matchExtractHeader(header, entry); err != nil {
			return Manifest{}, err
		}
		delete(remaining, header.Name)
		if entry.Type == symlinkType {
			target, err := rewriteLink(header.Linkname, opts.SourceHome)
			if err != nil {
				return Manifest{}, err
			}
			header.Linkname = target
			for i := range manifest.Entries {
				if manifest.Entries[i].Path == entry.Path {
					manifest.Entries[i].Target = target
					manifest.Entries[i].Size = int64(len(target))
					break
				}
			}
		}
		if entry.Type == fileType && strings.HasPrefix(entry.Path, pageRoot+"/") {
			parent, name, err := openParent(rootFD, header.Name, true)
			if err != nil {
				return Manifest{}, fmt.Errorf("open parent of %s: %w", header.Name, err)
			}
			var stat unix.Stat_t
			statErr := unix.Fstatat(parent, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
			closeErr := unix.Close(parent)
			if closeErr != nil {
				return Manifest{}, closeErr
			}
			if statErr == nil && stat.Mode&unix.S_IFMT != unix.S_IFREG {
				return Manifest{}, fmt.Errorf("existing journal is not a regular file: %s", header.Name)
			}
			if statErr != nil && !errors.Is(statErr, unix.ENOENT) {
				return Manifest{}, fmt.Errorf("inspect existing journal %s: %w", header.Name, statErr)
			}
			header.Name += ".incoming"
		}
		var content io.Reader = archive
		var rewritten *os.File
		if entry.Type == fileType {
			var expected ManifestEntry
			rewritten, expected, err = rewriteFile(archive, entry, rewriteFilePath(entry.Path), filepath.Join(root, filepath.FromSlash(rewriteTempRoot)), opts.SourceHome)
			if err != nil {
				return Manifest{}, fmt.Errorf("rewrite %s: %w", entry.Path, err)
			}
			content = rewritten
			header.Size = expected.Size
			for i := range manifest.Entries {
				if manifest.Entries[i].Path == entry.Path {
					manifest.Entries[i] = expected
					break
				}
			}
		}
		extractErr := extractEntry(content, header, rootFD)
		if rewritten != nil {
			extractErr = errors.Join(extractErr, rewritten.Close(), os.Remove(rewritten.Name()))
		}
		if extractErr != nil {
			return Manifest{}, extractErr
		}
	}
	if len(remaining) != 0 {
		return Manifest{}, errors.New("archive missing manifest entries")
	}
	if err := ownership(root, manifest); err != nil {
		return Manifest{}, err
	}
	if err := ReplaceJournals(root, manifest, opts.Layout.Journals.Names); err != nil {
		return Manifest{}, err
	}
	if hasRoot(manifest, pageRoot) {
		hasJournal := false
		for _, entry := range manifest.Entries {
			if entry.Type == fileType && strings.HasPrefix(entry.Path, pageRoot+"/") {
				hasJournal = true
				break
			}
		}
		if !hasJournal {
			dir := filepath.Join(root, filepath.FromSlash(pageRoot))
			items, err := os.ReadDir(dir)
			if err != nil {
				return Manifest{}, fmt.Errorf("list existing journals: %w", err)
			}
			for _, item := range items {
				if !strings.HasSuffix(item.Name(), ".jsonl") {
					continue
				}
				path := pageRoot + "/" + item.Name()
				entry, err := manifestEntry(filepath.Join(dir, item.Name()), path)
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				if err != nil {
					return Manifest{}, err
				}
				if entry.Type != fileType {
					return Manifest{}, fmt.Errorf("journal is not a regular file: %s", path)
				}
				manifest.Entries = append(manifest.Entries, entry)
			}
			sortManifest(&manifest)
		}
	}
	if err := compare(root, manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func matchExtractHeader(header *tar.Header, entry ManifestEntry) error {
	typeflag := map[string]byte{fileType: tar.TypeReg, directoryType: tar.TypeDir, symlinkType: tar.TypeSymlink}
	if header.Typeflag != typeflag[entry.Type] || uint32(header.Mode&0o777) != entry.Mode {
		return fmt.Errorf("archive metadata mismatch at %s", entry.Path)
	}
	if (entry.Type == fileType && header.Size != entry.Size) || (entry.Type != fileType && header.Size != 0) {
		return fmt.Errorf("archive size mismatch at %s", entry.Path)
	}
	if entry.Type == symlinkType && (header.Linkname != entry.Target || int64(len(header.Linkname)) != entry.Size) {
		return fmt.Errorf("archive symlink mismatch at %s", entry.Path)
	}
	return nil
}

func validateManifestRoots(manifest Manifest) error {
	if len(manifest.Roots) == 0 {
		return errors.New("manifest has no roots")
	}
	for _, root := range manifest.Roots {
		if err := validateRelativePath(root); err != nil {
			return err
		}
		if root != dataRoot && root != pageRoot {
			return fmt.Errorf("unsupported transfer root %s", root)
		}
	}
	return nil
}

func validateManifestPaths(manifest Manifest) error {
	for _, entry := range manifest.Entries {
		if err := validateManifestEntry(entry); err != nil {
			return err
		}
		if entry.Path == rewriteTempRoot || strings.HasPrefix(entry.Path, rewriteTempRoot+"/") || entry.Path == dataRoot+"/lost+found" || strings.HasPrefix(entry.Path, dataRoot+"/lost+found/") {
			return fmt.Errorf("reserved transfer path %s", entry.Path)
		}
		if !manifestContainsPath(manifest, entry.Path) {
			return fmt.Errorf("manifest path %s is outside declared roots", entry.Path)
		}
		if entry.Size < 0 || entry.Mode > 0o777 {
			return fmt.Errorf("manifest has invalid metadata at %s", entry.Path)
		}
		if entry.Type == fileType && len(entry.SHA256) != 64 {
			return fmt.Errorf("manifest has invalid sha256 at %s", entry.Path)
		}
		if entry.Type == symlinkType && entry.Target == "" {
			return fmt.Errorf("manifest has empty symlink target at %s", entry.Path)
		}
		if entry.Path == pageRoot {
			if entry.Type != directoryType {
				return errors.New("paging root must be a directory")
			}
			continue
		}
		if name, ok := strings.CutPrefix(entry.Path, "control/nemo-page/"); ok {
			if entry.Type != fileType || !strings.HasSuffix(name, ".jsonl") || strings.Contains(name, "/") {
				return fmt.Errorf("unsupported paging journal path %s", entry.Path)
			}
			if entry.Mode != 0o600 {
				return fmt.Errorf("invalid journal mode at %s", entry.Path)
			}
		}
	}
	return nil
}

func manifestContainsPath(manifest Manifest, name string) bool {
	for _, root := range manifest.Roots {
		if name == root || strings.HasPrefix(name, root+"/") {
			return true
		}
	}
	return false
}

func clearIncomingJournals(rootFD int, manifest Manifest) error {
	for _, entry := range manifest.Entries {
		if entry.Type != fileType || !strings.HasPrefix(entry.Path, pageRoot+"/") {
			continue
		}
		parent, name, err := openParent(rootFD, entry.Path+".incoming", false)
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return fmt.Errorf("open incoming journal parent: %w", err)
		}
		unlinkErr := unix.Unlinkat(parent, name, 0)
		if errors.Is(unlinkErr, unix.ENOENT) {
			unlinkErr = nil
		}
		if err := errors.Join(unlinkErr, unix.Close(parent)); err != nil {
			return fmt.Errorf("remove incoming journal %s: %w", entry.Path, err)
		}
	}
	return nil
}

// clearDataRoots removes only top-level data roots named by the manifest.
func clearDataRoots(rootFD int, manifest Manifest) error {
	roots := make(map[string]struct{})
	for _, entry := range manifest.Entries {
		rel, ok := strings.CutPrefix(entry.Path, dataRoot+"/")
		if !ok {
			continue
		}
		name, _, _ := strings.Cut(rel, "/")
		if name != ".nemo-move-tmp" {
			roots[name] = struct{}{}
		}
	}
	parent, _, err := openParent(rootFD, dataRoot+"/placeholder", false)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open data root: %w", err)
	}
	defer unix.Close(parent)
	for name := range roots {
		if err := removeTreeAt(parent, name); err != nil {
			return fmt.Errorf("remove data root %s: %w", name, err)
		}
	}
	return nil
}

func removeTreeAt(parent int, name string) error {
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
		return unix.Unlinkat(parent, name, 0)
	}
	if err != nil {
		return err
	}
	dir := os.NewFile(uintptr(fd), name)
	entries, readErr := dir.ReadDir(-1)
	if readErr == nil {
		for _, entry := range entries {
			if readErr = removeTreeAt(fd, entry.Name()); readErr != nil {
				break
			}
		}
	}
	if err := errors.Join(readErr, dir.Close()); err != nil {
		return err
	}
	return unix.Unlinkat(parent, name, unix.AT_REMOVEDIR)
}

// openParent walks without following links, optionally creating missing directories.
func openParent(rootFD int, name string, create bool) (int, string, error) {
	parts := strings.Split(name, "/")
	fd, err := unix.Dup(rootFD)
	if err != nil {
		return -1, "", err
	}
	for _, part := range parts[:len(parts)-1] {
		if create {
			if err := unix.Mkdirat(fd, part, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
				return -1, "", errors.Join(err, unix.Close(fd))
			}
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		closeErr := unix.Close(fd)
		if err != nil || closeErr != nil {
			if err == nil {
				closeErr = errors.Join(closeErr, unix.Close(next))
			}
			return -1, "", errors.Join(err, closeErr)
		}
		fd = next
	}
	return fd, parts[len(parts)-1], nil
}

func extractEntry(archive io.Reader, header *tar.Header, rootFD int) (resultErr error) {
	if header.Size < 0 || header.Size > maxInventoryBytes {
		return fmt.Errorf("invalid transfer entry size for %s", header.Name)
	}
	parent, name, err := openParent(rootFD, header.Name, true)
	if err != nil {
		return fmt.Errorf("open parent of %s: %w", header.Name, err)
	}
	defer func() { resultErr = errors.Join(resultErr, unix.Close(parent)) }()
	mode := uint32(header.Mode & 0o777)
	switch header.Typeflag {
	case tar.TypeDir:
		if err := unix.Mkdirat(parent, name, mode); err != nil && !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("create directory %s: %w", header.Name, err)
		}
		fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("open directory %s: %w", header.Name, err)
		}
		defer func() { resultErr = errors.Join(resultErr, unix.Close(fd)) }()
		return unix.Fchmod(fd, mode)
	case tar.TypeReg:
		fd, err := unix.Openat(parent, name, unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_WRONLY|unix.O_CLOEXEC, mode)
		if err != nil {
			return fmt.Errorf("create file %s: %w", header.Name, err)
		}
		file := os.NewFile(uintptr(fd), header.Name)
		_, copyErr := io.CopyN(file, archive, header.Size)
		modeErr := unix.Fchmod(fd, mode)
		closeErr := file.Close()
		if err := errors.Join(copyErr, modeErr, closeErr); err != nil {
			return fmt.Errorf("write file %s: %w", header.Name, err)
		}
		return nil
	case tar.TypeSymlink:
		clean := filepath.Clean(header.Linkname)
		if filepath.IsAbs(clean) && clean != "/opt/data" && !strings.HasPrefix(clean, "/opt/data/") {
			return fmt.Errorf("absolute symlink %s points outside the transfer tree", header.Name)
		}
		if err := unix.Symlinkat(header.Linkname, parent, name); err != nil {
			return fmt.Errorf("create symlink %s: %w", header.Name, err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported transfer entry type at %s", header.Name)
	}
}

func applyOwnership(root string, manifest Manifest) (resultErr error) {
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open transfer root: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, unix.Close(rootFD)) }()
	for _, entry := range manifest.Entries {
		uid, gid := 10000, 10000
		if entry.Path == pageRoot {
			uid, gid = 0, 0
		}
		owned := entry
		if entry.Type == fileType && strings.HasPrefix(entry.Path, pageRoot+"/") {
			owned.Path += ".incoming"
		}
		if err := ownEntry(rootFD, owned, uid, gid); err != nil {
			return fmt.Errorf("set ownership for %s: %w", entry.Path, err)
		}
	}
	return nil
}

func ownEntry(rootFD int, entry ManifestEntry, uid, gid int) (resultErr error) {
	mode := entry.Mode
	if entry.Path == pageRoot {
		mode = 0o755
	}
	parent, name, err := openParent(rootFD, entry.Path, false)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, unix.Close(parent)) }()
	if entry.Type == symlinkType {
		var stat unix.Stat_t
		if err := unix.Fstatat(parent, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFLNK {
			return errors.New("expected symlink")
		}
		return unix.Fchownat(parent, name, uid, gid, unix.AT_SYMLINK_NOFOLLOW)
	}
	flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
	if entry.Type == directoryType {
		flags |= unix.O_DIRECTORY
	}
	fd, err := unix.Openat(parent, name, flags, 0)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, unix.Close(fd)) }()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if (entry.Type == fileType && stat.Mode&unix.S_IFMT != unix.S_IFREG) || (entry.Type == directoryType && stat.Mode&unix.S_IFMT != unix.S_IFDIR) {
		return errors.New("unexpected file type")
	}
	if err := unix.Fchown(fd, uid, gid); err != nil {
		return err
	}
	return unix.Fchmod(fd, mode)
}
