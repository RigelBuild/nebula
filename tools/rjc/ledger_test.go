package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLedgerReplacePrunesOnlyStaleTarget(t *testing.T) {
	ledger := Ledger{root: t.TempDir()}
	target := LedgerEntry{Repo: "owner/repo", PR: 1, Bookmark: "lane/topic", Codename: "lane"}
	otherBookmark := LedgerEntry{Repo: target.Repo, PR: 5, Bookmark: "lane/other", Codename: "lane"}
	otherRepo := LedgerEntry{Repo: "owner/other", PR: 1, Bookmark: target.Bookmark, Codename: "lane"}
	for _, entry := range []LedgerEntry{target, otherBookmark, otherRepo} {
		if err := ledger.Save(entry); err != nil {
			t.Fatal(err)
		}
	}

	replacement := LedgerEntry{Repo: target.Repo, PR: 2, Bookmark: target.Bookmark, Codename: target.Codename}
	if err := ledger.Save(replacement); err != nil {
		t.Fatal(err)
	}
	if err := ledger.pruneOthers(replacement.Repo, replacement.Bookmark, replacement.PR); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name        string
		entry       LedgerEntry
		wantMissing bool
	}{
		{name: "stale target", entry: target, wantMissing: true},
		{name: "other bookmark", entry: otherBookmark},
		{name: "other repo", entry: otherRepo},
		{name: "replacement", entry: replacement},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, err := ledger.path(tc.entry.Repo, tc.entry.PR)
			if err != nil {
				t.Fatal(err)
			}
			_, err = os.Stat(path)
			if tc.wantMissing {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("stat stale file error = %v, want not-exist", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("stat retained file: %v", err)
			}
		})
	}
}

func TestLedgerReplaceRejectsCorruptEntry(t *testing.T) {
	ledger := Ledger{root: t.TempDir()}
	corruptPath := filepath.Join(ledger.root, "prs", "owner", "repo", "7.json")
	if err := os.MkdirAll(filepath.Dir(corruptPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corruptPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	replacement := LedgerEntry{Repo: "owner/repo", PR: 2, Bookmark: "lane/topic"}
	if err := ledger.Save(replacement); err != nil {
		t.Fatal(err)
	}
	if err := ledger.pruneOthers(replacement.Repo, replacement.Bookmark, replacement.PR); err == nil {
		t.Fatal("pruneOthers accepted corrupt JSON")
	}
	if _, err := os.Stat(corruptPath); err != nil {
		t.Fatalf("corrupt file was not kept: %v", err)
	}
}

func TestLedgerReplaceRejectsEntryPathMismatch(t *testing.T) {
	ledger := Ledger{root: t.TempDir()}
	entry := LedgerEntry{Repo: "owner/repo", PR: 1, Bookmark: "lane/topic"}
	if err := ledger.Save(entry); err != nil {
		t.Fatal(err)
	}
	mismatchPath := filepath.Join(ledger.root, "prs", "owner", "repo", "7.json")
	if err := os.WriteFile(mismatchPath, []byte(`{"repo":"owner/repo","pr":1,"bookmark":"lane/topic"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	replacement := LedgerEntry{Repo: entry.Repo, PR: 2, Bookmark: entry.Bookmark}
	if err := ledger.Save(replacement); err != nil {
		t.Fatal(err)
	}
	err := ledger.pruneOthers(replacement.Repo, replacement.Bookmark, replacement.PR)
	if err == nil || err.Error() != "ledger entry does not match its path" {
		t.Fatalf("pruneOthers error = %v, want ledger entry path mismatch", err)
	}
	if _, err := os.Stat(mismatchPath); err != nil {
		t.Fatalf("mismatched file was not kept: %v", err)
	}
	entryPath, err := ledger.path(entry.Repo, entry.PR)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(entryPath); err != nil {
		t.Fatalf("valid entry was unexpectedly removed: %v", err)
	}
}

func TestLedgerRecordPushUpdatesLastPushedHead(t *testing.T) {
	ledger := Ledger{root: t.TempDir()}
	entry := LedgerEntry{Repo: "owner/repo", PR: 7, Bookmark: "lane/topic", Codename: "lane"}
	if err := ledger.Save(entry); err != nil {
		t.Fatal(err)
	}
	if err := ledger.recordPush(entry.Repo, entry.Bookmark, entry.PR, entry.Codename, "first-head"); err != nil {
		t.Fatal(err)
	}
	got, found, err := ledger.Find(entry.Repo, entry.Bookmark)
	if err != nil || !found || got.LastPushedHead != "first-head" {
		t.Fatalf("ledger entry = %+v, found=%v, err=%v", got, found, err)
	}
	if err := ledger.recordPush(entry.Repo, entry.Bookmark, entry.PR, entry.Codename, "next-head"); err != nil {
		t.Fatal(err)
	}
	got, found, err = ledger.Find(entry.Repo, entry.Bookmark)
	if err != nil || !found || got.LastPushedHead != "next-head" {
		t.Fatalf("updated ledger entry = %+v, found=%v, err=%v", got, found, err)
	}
}
