package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadRowsFirstRowWinsAcrossTwoFiles(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"2026-11-01.jsonl": "{\"v\":1,\"kind\":\"message\",\"ts\":\"2026-11-01T00:00:00Z\",\"row_key\":\"same\",\"session_id\":\"first\",\"input\":1}\n",
		"2026-11-02.jsonl": "{\"v\":1,\"kind\":\"message\",\"ts\":\"2026-11-02T00:00:00Z\",\"row_key\":\"same\",\"session_id\":\"second\",\"input\":2}\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	rows, skipped, err := ReadRows(dir, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 0 || len(rows) != 1 {
		t.Fatalf("ReadRows() = %d rows, %d skipped; want 1 row, 0 skipped", len(rows), skipped)
	}
	if rows[0].SessionID != "first" || rows[0].Input == nil || *rows[0].Input != 1 {
		t.Fatalf("first row = %+v; duplicate in later file replaced it", rows[0])
	}
}

func TestReadRowsUnknownVersionIsSkippedAndCounted(t *testing.T) {
	dir := t.TempDir()
	content, err := os.ReadFile(filepath.Join("testdata", "unknown-version.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "2026-11-03.jsonl"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	date := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	rows, skipped, err := ReadRows(dir, date, date)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 || skipped != 1 {
		t.Fatalf("ReadRows() = %d rows, %d skipped; want 0 rows, 1 skipped", len(rows), skipped)
	}
}
