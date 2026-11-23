package inventory

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/config"
)

func (s *walkState) addOutsideTargets(source config.Source, rootPath string) error {
	if filepath.Base(source.Path) != "custom-session-files" {
		return nil
	}
	return filepath.WalkDir(rootPath, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walking custom session markers: %w", walkErr)
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		marker, err := os.Open(path) //nolint:gosec // Paths come from the configured walk roots.
		if err != nil {
			return fmt.Errorf("read custom session marker: %w", err)
		}
		return scanMarker(marker, s)
	})
}

func scanMarker(marker *os.File, state *walkState) error {
	scanner := bufio.NewScanner(marker)
	for scanner.Scan() {
		target := strings.TrimSpace(scanner.Text())
		if target == "" || !filepath.IsAbs(target) {
			continue
		}
		if _, err := os.Lstat(target); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			closeErr := marker.Close()
			return errors.Join(fmt.Errorf("stat custom session target: %w", err), closeErr)
		}
		inside, err := filepath.Rel(state.cfg.Root, target)
		if err != nil {
			closeErr := marker.Close()
			return errors.Join(fmt.Errorf("classify custom session target: %w", err), closeErr)
		}
		if inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
			state.inventory.OutsideTargets++
		}
	}
	scanErr := scanner.Err()
	closeErr := marker.Close()
	return errors.Join(scanErr, closeErr)
}
