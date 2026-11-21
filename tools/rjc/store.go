package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/samber/oops"
)

type fileBaseline struct {
	root string
	keys issueKeys
}

func (s fileBaseline) Load(bookmark string) (Published, bool, error) {
	path, err := s.path(bookmark, ".published.md")
	if err != nil {
		return Published{}, false, err
	}
	body, err := os.ReadFile(path) //nolint:gosec // path validates the bookmark before choosing a published state file.
	if errors.Is(err, os.ErrNotExist) {
		return Published{}, false, nil
	}
	if err != nil {
		return Published{}, false, oops.Wrapf(err, "read published baseline")
	}
	title, authored, ok := strings.Cut(string(body), "\n\n")
	if !ok || title == "" {
		return Published{}, false, oops.Errorf("invalid published baseline; inspect it before submitting")
	}
	return Published{Title: title, Authored: authored}, true, nil
}
func (s fileBaseline) Commit(bookmark string, p Published) error {
	path, err := s.path(bookmark, ".published.md")
	if err != nil {
		return err
	}
	return atomicWrite(path, p.Title+"\n\n"+p.Authored)
}
func (s fileBaseline) path(bookmark, suffix string) (string, error) {
	if !s.keys.branchName(bookmark).Pass {
		return "", oops.Errorf("invalid bookmark for metadata storage")
	}
	return filepath.Join(s.root, "bodies", filepath.FromSlash(bookmark)+suffix), nil
}
func (s fileBaseline) candidate(bookmark, body string) (string, error) {
	path, err := s.path(bookmark, ".candidate.md")
	if err != nil {
		return "", err
	}
	return path, atomicWrite(path, body)
}
func atomicWrite(path, contents string) (err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return oops.Wrapf(err, "create metadata directory")
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".rjc-*")
	if err != nil {
		return oops.Wrapf(err, "create metadata file")
	}
	defer func() {
		removeErr := os.Remove(file.Name())
		if !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	if _, err := file.WriteString(contents); err != nil {
		return oops.Wrapf(errors.Join(err, file.Close()), "write metadata file")
	}
	if err := file.Close(); err != nil {
		return oops.Wrapf(err, "close metadata file")
	}
	return oops.Wrapf(os.Rename(file.Name(), path), "publish metadata file")
}
