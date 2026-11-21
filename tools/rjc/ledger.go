package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

type LedgerEntry struct {
	Repo           string    `json:"repo"`
	PR             int       `json:"pr"`
	Bookmark       string    `json:"bookmark"`
	Codename       string    `json:"codename"`
	Created        time.Time `json:"created"`
	LastPushedHead string    `json:"lastPushedHead,omitempty"`
	// PushedHeads lists every head rjc pushed for this PR, oldest first. Old
	// ledgers without it still read: LastPushedHead stays the adoption record.
	PushedHeads []string `json:"pushedHeads,omitempty"`
}

type Ledger struct{ root string }

func (l Ledger) Save(e LedgerEntry) error {
	path, err := l.path(e.Repo, e.PR)
	if err != nil {
		return err
	}
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encode ledger: %w", err)
	}
	return atomicWrite(path, string(data))
}

func (l Ledger) Find(repo, bookmark string) (LedgerEntry, bool, error) {
	entries, err := l.entries(repo)
	if err != nil {
		return LedgerEntry{}, false, err
	}
	for _, item := range entries {
		if item.entry.Bookmark == bookmark {
			return item.entry, true, nil
		}
	}
	return LedgerEntry{}, false, nil
}

func (l Ledger) path(repo string, pr int) (string, error) {
	dir, err := stateRepoPath(l.root, "prs", repo)
	if err != nil {
		return "", err
	}
	if pr <= 0 {
		return "", errors.New("invalid PR number")
	}
	return filepath.Join(dir, strconv.Itoa(pr)+".json"), nil
}
func (l Ledger) findForPR(repo, bookmark string, pr int) (LedgerEntry, bool, error) {
	entries, err := l.entries(repo)
	if err != nil {
		return LedgerEntry{}, false, err
	}
	var found LedgerEntry
	matched := false
	for _, item := range entries {
		if item.entry.Bookmark != bookmark {
			continue
		}
		if item.entry.PR == pr {
			return item.entry, true, nil
		}
		if !matched {
			found = item.entry
			matched = true
		}
	}
	return found, matched, nil
}
func (l Ledger) entries(repo string) ([]ledgerFileEntry, error) {
	dir, err := stateRepoPath(l.root, "prs", repo)
	if err != nil {
		return nil, err
	}
	files, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []ledgerFileEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	entries := make([]ledgerFileEntry, 0, len(files))
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, file.Name())
		data, err := os.ReadFile(path) //nolint:gosec // ReadDir returns names under the validated repository directory.
		if err != nil {
			return nil, err
		}
		var entry LedgerEntry
		if err := json.Unmarshal(data, &entry); err != nil {
			return nil, err
		}
		expected, err := l.path(entry.Repo, entry.PR)
		if err != nil {
			return nil, err
		}
		if expected != path {
			return nil, errors.New("ledger entry does not match its path")
		}
		if entry.Repo != repo {
			continue
		}
		entries = append(entries, ledgerFileEntry{path: path, entry: entry})
	}
	return entries, nil
}

// pruneOthers runs after callers save the kept entry, so the bookmark is never without an entry.
func (l Ledger) pruneOthers(repo, bookmark string, keepPR int) error {
	entries, err := l.entries(repo)
	if err != nil {
		return err
	}
	for _, item := range entries {
		if item.entry.Bookmark != bookmark || item.entry.PR == keepPR {
			continue
		}
		if err := os.Remove(item.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
func (l Ledger) recordPush(repo, bookmark string, pr int, codename, head string) error {
	entry, found, err := l.findForPR(repo, bookmark, pr)
	if err != nil {
		return err
	}
	if !found || entry.PR != pr {
		entry = LedgerEntry{Repo: repo, PR: pr, Bookmark: bookmark, Codename: codename, Created: time.Now().UTC()}
	}
	entry.LastPushedHead = head
	if head != "" && !slices.Contains(entry.PushedHeads, head) {
		entry.PushedHeads = append(entry.PushedHeads, head)
	}
	if err := l.Save(entry); err != nil {
		return err
	}
	return l.pruneOthers(repo, bookmark, pr)
}

type ledgerFileEntry struct {
	path  string
	entry LedgerEntry
}
