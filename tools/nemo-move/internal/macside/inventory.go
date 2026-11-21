// Package macside inventories and exports the source host's durable home state.
package macside

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/RigelBuild/nebula/tools/nemo-move/internal/layout"
)

const copyUnknown = "Copy (unknown)"

// Row describes a durable-state source and its destination.
// Paths are relative to home.
type Row = layout.Row

// RowReport records the readable bytes for a copy root.
type RowReport struct {
	Row      Row    `json:"row"`
	Bytes    int64  `json:"bytes"`
	Readable bool   `json:"readable"`
	Error    string `json:"error,omitempty"`
}

// Report is the inventory of home transfer roots.
type Report struct {
	Rows       []RowReport `json:"rows"`
	TotalBytes int64       `json:"totalBytes"`
	Time       time.Time   `json:"time"`
}

var maxBytes int64 = 30 << 30

// Inventory checks every copy row in the layout, including staged Google
// credentials and any unknown children that the layout asks it to discover.
func Inventory(home string, lay layout.Layout, now time.Time) (Report, error) {
	report := Report{Rows: []RowReport{}, Time: now}
	absHome, err := filepath.Abs(home)
	if err != nil {
		return report, fmt.Errorf("resolve home: %w", err)
	}
	var failures []error
	rows, discoverErr := expandRows(absHome, lay)
	failures = append(failures, discoverErr)
	for _, row := range rows {
		item := RowReport{Row: row}
		if row.Kind != layout.Copy && row.Kind != copyUnknown || filepath.IsAbs(row.Path) {
			report.Rows = append(report.Rows, item)
			continue
		}
		if row.Kind == copyUnknown {
			err := walkRow(absHome, row, func(path string, info fs.FileInfo) error {
				size, err := checkEntry(absHome, path, info)
				item.Bytes += size
				return err
			})
			item.Readable = err == nil
			if err != nil {
				item.Error = err.Error()
				failures = append(failures, fmt.Errorf("inventory %s: %w", row.Path, err))
			}
			report.Rows = append(report.Rows, item)
			continue
		}
		err := walkRow(absHome, row, func(path string, info fs.FileInfo) error {
			size, err := checkEntry(absHome, path, info)
			if err != nil {
				return err
			}
			if size > maxBytes-report.TotalBytes {
				return fmt.Errorf("copy rows exceed %d bytes", maxBytes)
			}
			item.Bytes += size
			report.TotalBytes += size
			return nil
		})
		item.Readable = err == nil
		if err != nil {
			item.Error = err.Error()
			if strings.HasPrefix(row.Path, layout.StagingDir+"/") {
				item.Error = "needs staging: " + item.Error
			}
			failures = append(failures, fmt.Errorf("inventory %s: %w", row.Path, err))
		}
		report.Rows = append(report.Rows, item)
	}
	return report, errors.Join(failures...)
}

// expandRows returns the layout rows plus one unknown copy row for each child
// that a discover entry finds and no row covers.
func expandRows(absHome string, lay layout.Layout) ([]Row, error) {
	var (
		rows     []Row
		failures []error
	)
	for _, row := range lay.Rows {
		rows = append(rows, row)
		for _, discover := range row.Discover {
			dir := filepath.Join(absHome, row.Path, discover.Dir)
			children, err := os.ReadDir(dir)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				failures = append(failures, fmt.Errorf("inventory %s: %w", filepath.Join(row.Path, discover.Dir), err))
				continue
			}
			known := make(map[string]bool, len(discover.Known))
			for _, name := range discover.Known {
				known[name] = true
			}
			for _, child := range children {
				if known[child.Name()] {
					continue
				}
				path := filepath.ToSlash(filepath.Join(row.Path, discover.Dir, child.Name()))
				rows = append(rows, Row{Path: path, Kind: copyUnknown, Dest: row.Dest + "/" + strings.TrimPrefix(path, row.Path+"/")})
			}
		}
	}
	return rows, errors.Join(failures...)
}

func walkRow(home string, row Row, visit func(string, fs.FileInfo) error) error {
	root := filepath.Join(home, row.Path)
	skip := ownedElsewhere(row)
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != root && len(skip) > 0 {
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			if slicesContains(skip, filepath.ToSlash(rel)) {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return visit(path, info)
	})
}

// ownedElsewhere lists the paths under row that another row covers: the Skip
// entries and each discover-known child. A path has one owning row.
func ownedElsewhere(row Row) []string {
	skip := append([]string(nil), row.Skip...)
	for _, discover := range row.Discover {
		for _, name := range discover.Known {
			skip = append(skip, filepath.ToSlash(filepath.Join(discover.Dir, name)))
		}
	}
	return skip
}

func slicesContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func checkEntry(home, path string, info fs.FileInfo) (int64, error) {
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return 0, err
		}
		if filepath.IsAbs(target) {
			rel, err := filepath.Rel(home, filepath.Clean(target))
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return 0, fmt.Errorf("symlink %s points outside home", path)
			}
		}
		return 0, nil
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return 0, fmt.Errorf("unsupported file type at %s", path)
	}
	file, err := os.Open(path) //nolint:gosec // Path comes from the home tree walk.
	if err != nil {
		return 0, err
	}
	if info.IsDir() {
		_, err = file.Readdirnames(-1)
	}
	closeErr := file.Close()
	if err != nil {
		return 0, err
	}
	if closeErr != nil {
		return 0, closeErr
	}
	if info.IsDir() {
		return 0, nil
	}
	return info.Size(), nil
}
