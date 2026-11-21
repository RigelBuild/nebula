// Package macside inventories and exports Nemo's durable home state.
package macside

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	Copy        = "Copy"
	Rebuild     = "Rebuild"
	Retire      = "Retire"
	copyUnknown = "Copy (unknown)"
)

// Row describes a durable-state source and its destination.
// Paths are relative to home except for the separately transferred page journals.
type Row struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
	Dest string `json:"dest"`
}

// Rows returns the durable-state table. The nemo root also covers unknown children.
func Rows() []Row {
	return []Row{
		{Path: ".hermes", Kind: Copy, Dest: "/opt/data"},
		{Path: "nemo", Kind: Copy, Dest: "/opt/data/nemo"},
		{Path: "nemo/health", Kind: Copy, Dest: "/opt/data/nemo/health"},
		{Path: "nemo/finance", Kind: Copy, Dest: "/opt/data/nemo/finance"},
		{Path: "nemo/archive", Kind: Copy, Dest: "/opt/data/nemo/archive"},
		{Path: "nemo/state", Kind: Copy, Dest: "/opt/data/nemo/state"},
		{Path: "nemo/repos/RigelBuild", Kind: Copy, Dest: "/opt/data/nemo/repos/RigelBuild"},
		{Path: ".local/state/nemo-move-staging/workspace/credentials", Kind: Copy, Dest: "/opt/data/.local/state/nemo-google-mcp/workspace/credentials"},
		{Path: ".local/state/nemo-move-staging/personal/credentials", Kind: Copy, Dest: "/opt/data/.local/state/nemo-google-mcp/personal/credentials"},
		{Path: "/var/lib/nemo-page/*.jsonl", Kind: Copy, Dest: "/control/nemo-page"},
		{Path: ".local/state/nemo-linear", Kind: Rebuild, Dest: "Secret nemo-linear-token"},
		{Path: ".local/state/nemo-cluster-access", Kind: Retire, Dest: "projected token, ExternalSecret"},
		{Path: ".config/op", Kind: Rebuild, Dest: "/opt/data/.config/op"},
		{Path: "Library/Application Support/Nemo/Chrome", Kind: Rebuild, Dest: "/opt/data/chrome"},
		{Path: "Library/Application Support/Nemo/caddy", Kind: Retire, Dest: "none"},
	}
}

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

// Inventory checks all home copy roots, including staged Google credentials.
func Inventory(home string, now time.Time) (Report, error) {
	report := Report{Rows: []RowReport{}, Time: now}
	absHome, err := filepath.Abs(home)
	if err != nil {
		return report, fmt.Errorf("resolve home: %w", err)
	}
	var failures []error
	rows := Rows()
	children, readErr := os.ReadDir(filepath.Join(absHome, "nemo"))
	if readErr == nil {
		for _, child := range children {
			switch child.Name() {
			case "health", "finance", "archive", "state", "repos":
				continue
			}
			path := "nemo/" + child.Name()
			rows = append(rows, Row{Path: path, Kind: copyUnknown, Dest: "/opt/data/" + path})
		}
	}
	owners, err := os.ReadDir(filepath.Join(absHome, "nemo/repos"))
	if err == nil {
		for _, owner := range owners {
			if owner.Name() != "RigelBuild" {
				path := "nemo/repos/" + owner.Name()
				rows = append(rows, Row{Path: path, Kind: copyUnknown, Dest: "/opt/data/" + path})
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		failures = append(failures, fmt.Errorf("inventory nemo/repos: %w", err))
	}
	for _, row := range rows {
		item := RowReport{Row: row}
		if row.Kind != Copy && row.Kind != copyUnknown || filepath.IsAbs(row.Path) {
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
			if strings.Contains(row.Path, "nemo-move-staging") {
				item.Error = "needs staging: " + item.Error
			}
			failures = append(failures, fmt.Errorf("inventory %s: %w", row.Path, err))
		}
		report.Rows = append(report.Rows, item)
	}
	return report, errors.Join(failures...)
}

func walkRow(home string, row Row, visit func(string, fs.FileInfo) error) error {
	root := filepath.Join(home, row.Path)
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if row.Path == "nemo" && path != root {
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			switch filepath.ToSlash(rel) {
			case "health", "finance", "archive", "state", "repos/RigelBuild":
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		if row.Path == ".hermes" && path != root {
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			switch rel {
			case "gateway_state.json", "gateway.pid", "cron.pid", "gateway.lock", "processes.json", "nemo-unlock":
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
