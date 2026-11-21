package inventory

import (
	"fmt"
	"os"
	"path/filepath"
)

func (s *walkState) validateWatchRoots() error {
	paths := make(map[string]struct{}, len(s.inventory.Lines))
	for _, line := range s.inventory.Lines {
		paths[line.Path] = struct{}{}
	}
	for _, watch := range s.cfg.Watch {
		watchPath := filepath.Join(s.cfg.Root, filepath.FromSlash(watch))
		if err := filepath.WalkDir(watchPath, func(path string, entry os.DirEntry, walkErr error) error {
			return s.validateWatchEntry(path, entry, walkErr, paths)
		}); err != nil {
			return fmt.Errorf("validate watch root: %w", err)
		}
	}
	return nil
}

func (s *walkState) validateWatchEntry(path string, entry os.DirEntry, walkErr error, paths map[string]struct{}) error {
	if walkErr != nil {
		if os.IsNotExist(walkErr) {
			return nil
		}
		return fmt.Errorf("walking watch root: %w", walkErr)
	}
	if entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
		return nil
	}
	relative, err := filepath.Rel(s.cfg.Root, path)
	if err != nil {
		return fmt.Errorf("relative watch path: %w", err)
	}
	excluded, err := s.excluded(filepath.ToSlash(relative))
	if err != nil {
		return err
	}
	if excluded {
		return nil
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	if _, ok := paths[clean]; !ok {
		s.inventory.Errors = append(s.inventory.Errors, AssignError{Path: clean, Reason: "unknown watched file"})
	}
	return nil
}
