package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func (l Ledger) List(keys issueKeys, codename, repo string) ([]LedgerEntry, error) {
	entries := []LedgerEntry{}
	root := filepath.Join(l.root, "prs")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		data, err := os.ReadFile(path) //nolint:gosec // WalkDir confines files to the local state directory.
		if err != nil {
			return err
		}
		var entry LedgerEntry
		if err := json.Unmarshal(data, &entry); err != nil {
			return err
		}
		expected, err := l.path(entry.Repo, entry.PR)
		if err != nil {
			return err
		}
		if expected != path {
			return errors.New("ledger entry does not match its path")
		}
		if entry.Codename == codename && keys.Owned(codename, entry.Bookmark) && (repo == "" || repo == entry.Repo) {
			entries = append(entries, entry)
		}
		return nil
	})
	return entries, err
}
