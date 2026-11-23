package inventory

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/config"
)

type walkState struct {
	cfg       config.Walk
	previous  map[string]Line
	inventory Inventory
	stems     map[string]string
	absolute  map[string]string
}

// Walk scans configured roots without following symlinks and compares file content to previous.
func Walk(cfg config.Walk, previous *Inventory) (Inventory, Delta, error) {
	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return Inventory{}, Delta{}, fmt.Errorf("inventory: resolve root: %w", err)
	}
	cfg.Root = root
	state := walkState{
		cfg:       cfg,
		previous:  make(map[string]Line),
		inventory: Inventory{Lines: []Line{}, Dangling: []Dangling{}, Excluded: []Excluded{}, Errors: []AssignError{}},
		stems:     make(map[string]string),
		absolute:  make(map[string]string),
	}
	if previous != nil {
		for _, line := range previous.Lines {
			state.previous[lineKey(line)] = line
		}
	}
	if err := state.walkSources(); err != nil {
		return Inventory{}, Delta{}, err
	}
	state.assignRecords()
	state.checkClosure()
	sortLines(state.inventory.Lines)
	delta := Diff(previous, state.inventory)
	return state.inventory, delta, nil
}

// Diff reports added, changed and removed file versions between two inventories.
func Diff(previous *Inventory, now Inventory) Delta {
	delta := Delta{Added: []Line{}, Changed: []Line{}, Removed: []Line{}}
	if previous == nil {
		delta.Added = append(delta.Added, now.Lines...)
		return delta
	}
	oldLines := make(map[string]Line, len(previous.Lines))
	for _, line := range previous.Lines {
		oldLines[lineKey(line)] = line
	}
	newLines := make(map[string]Line, len(now.Lines))
	for _, line := range now.Lines {
		key := lineKey(line)
		newLines[key] = line
		old, ok := oldLines[key]
		if !ok {
			delta.Added = append(delta.Added, line)
			continue
		}
		if !sameContent(old, line) {
			delta.Changed = append(delta.Changed, line)
		}
	}
	for key, line := range oldLines {
		if _, ok := newLines[key]; !ok {
			delta.Removed = append(delta.Removed, line)
		}
	}
	sortLines(delta.Added)
	sortLines(delta.Changed)
	sortLines(delta.Removed)
	return delta
}

func (s *walkState) walkSources() error {
	if len(s.cfg.Sources) == 0 {
		return errors.New("inventory: no source roots configured")
	}
	for _, source := range s.cfg.Sources {
		pattern, err := s.sourcePath(source.Path)
		if err != nil {
			return fmt.Errorf("inventory: source %q: %w", source.Name, err)
		}
		roots, err := filepath.Glob(pattern)
		if err != nil {
			return fmt.Errorf("inventory: source pattern: %w", err)
		}
		for _, rootPath := range roots {
			if err := s.walkSource(source, rootPath); err != nil {
				return err
			}
		}
	}
	return s.validateWatchRoots()
}

func (s *walkState) walkSource(source config.Source, rootPath string) error {
	info, err := os.Lstat(rootPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inventory: source %q: %w", source.Name, err)
	}
	if err := s.addOutsideTargets(source, rootPath); err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		relative, err := filepath.Rel(s.cfg.Root, rootPath)
		if err != nil {
			return fmt.Errorf("relative source path: %w", err)
		}
		return s.addFile(source.Name, rootPath, filepath.ToSlash(relative), info)
	}
	return filepath.WalkDir(rootPath, func(path string, entry os.DirEntry, walkErr error) error {
		return s.walkSourceEntry(source.Name, rootPath, path, entry, walkErr)
	})
}

func (s *walkState) walkSourceEntry(source, rootPath, path string, entry os.DirEntry, walkErr error) error {
	if walkErr != nil {
		return fmt.Errorf("walking source %q: %w", source, walkErr)
	}
	if path == rootPath {
		return nil
	}
	relative, err := filepath.Rel(s.cfg.Root, path)
	if err != nil {
		return fmt.Errorf("relative source path: %w", err)
	}
	relative = filepath.ToSlash(relative)
	excluded, err := s.excluded(relative)
	if err != nil {
		return err
	}
	if excluded {
		if entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
			return filepath.SkipDir
		}
		return s.addExcluded(relative, path)
	}
	if entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
		return nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat source entry: %w", err)
	}
	return s.addFile(source, path, relative, info)
}

func (s *walkState) sourcePath(path string) (string, error) {
	return SourcePath(s.cfg, path)
}

// SourcePath resolves a configured source path (absolute, ~/, or relative to cfg.Root) to its glob pattern.
func SourcePath(cfg config.Walk, path string) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		return filepath.Join(home, filepath.FromSlash(path[2:])), nil
	}
	return filepath.Join(cfg.Root, filepath.FromSlash(path)), nil
}
func (s *walkState) excluded(path string) (bool, error) {
	for _, exclusion := range s.cfg.Exclude {
		pattern := filepath.ToSlash(exclusion.Path)
		matched, err := filepath.Match(pattern, path)
		if err != nil {
			return false, fmt.Errorf("invalid exclusion pattern: %w", err)
		}
		underDirectory := strings.HasPrefix(path, strings.TrimSuffix(pattern, "/")+"/")
		if matched || underDirectory {
			return true, nil
		}
	}
	return false, nil
}

func (s *walkState) addExcluded(_ string, path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat excluded entry: %w", err)
	}
	if len(s.inventory.Excluded) == 0 {
		s.inventory.Excluded = append(s.inventory.Excluded, Excluded{})
	}
	s.inventory.Excluded[0].Count++
	s.inventory.Excluded[0].Bytes += info.Size()
	return nil
}

func (s *walkState) addFile(source, absolutePath, path string, info os.FileInfo) error {
	line := Line{
		Root:    source,
		Path:    filepath.ToSlash(filepath.Clean(absolutePath)),
		Type:    fileType(info.Mode()),
		Class:   "normal",
		Size:    info.Size(),
		MtimeNs: info.ModTime().UnixNano(),
	}
	if line.Type == "symlink" {
		target, err := os.Readlink(absolutePath)
		if err != nil {
			return fmt.Errorf("read symlink: %w", err)
		}
		line.LinkTarget = target
		line.SHA256 = sha256.Sum256([]byte(target))
	} else {
		if isFinalOnly(path, s.cfg.FinalOnly) {
			line.Class = "final-only"
		}
		if previous, ok := s.previous[lineKey(line)]; ok && previous.Size == line.Size && previous.MtimeNs == line.MtimeNs {
			line.SHA256 = previous.SHA256
		} else {
			hash, err := hashFile(absolutePath)
			if err != nil {
				return fmt.Errorf("hash source entry: %w", err)
			}
			line.SHA256 = hash
		}
	}
	if strings.HasSuffix(strings.ToLower(line.Path), ".jsonl") {
		line.SessionID = sessionID(absolutePath, line.Path)
	}
	s.inventory.Lines = append(s.inventory.Lines, line)
	s.absolute[line.Path] = absolutePath
	return nil
}

func hashFile(path string) ([32]byte, error) {
	file, err := os.Open(path) //nolint:gosec // Paths come from the configured walk roots.
	if err != nil {
		return [32]byte{}, err
	}
	defer func() {
		_ = file.Close()
	}()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return [32]byte{}, err
	}
	var result [32]byte
	copy(result[:], hash.Sum(nil))
	return result, nil
}

func fileType(mode os.FileMode) string {
	if mode&os.ModeSymlink != 0 {
		return "symlink"
	}
	if mode.IsDir() {
		return "directory"
	}
	if mode.IsRegular() {
		return "file"
	}
	return "other"
}

func isFinalOnly(path string, entries []string) bool {
	for _, entry := range entries {
		matched, err := filepath.Match(filepath.ToSlash(entry), filepath.ToSlash(path))
		if err == nil && matched {
			return true
		}
	}
	return false
}
