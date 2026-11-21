package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunReportJSON(t *testing.T) {
	root := t.TempDir()
	content := "{\"v\":1,\"kind\":\"message\",\"ts\":\"2026-11-01T00:00:00Z\",\"row_key\":\"one\",\"session_id\":\"s\",\"issue\":null}\n"
	if err := os.WriteFile(filepath.Join(root, "2026-11-01.jsonl"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	args := []string{"report", "--from", "2026-11-01", "--to", "2026-11-01", "--by", "issue", "--dir", root, "--json"}
	err := runReport(args[1:], &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	var report reportJSON
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Groups) != 1 || report.Groups[0].Key[Key("issue")] != "untagged" {
		t.Fatalf("report groups = %+v, want one untagged group", report.Groups)
	}
}

func TestLoadPricesUsesEmbeddedDefault(t *testing.T) {
	table, err := LoadPrices("")
	if err != nil {
		t.Fatal(err)
	}
	if len(table) != 0 {
		t.Fatalf("embedded prices has %d entries, want empty table", len(table))
	}
}

func TestRunOutcomesUsesThirtyDayInclusiveWindow(t *testing.T) {
	root := t.TempDir()
	date := time.Date(2026, 11, 9, 23, 30, 0, 0, time.FixedZone("local", -7*60*60))
	utcDate := utcDay(date)
	start, end := outcomeWindow(utcDate)
	for _, day := range []time.Time{utcDate.AddDate(0, 0, -30), start, end} {
		content := "{\"v\":1,\"kind\":\"message\",\"ts\":\"" + day.Format(time.RFC3339) + "\",\"row_key\":\"" + day.Format("2006-01-02") + "\",\"session_id\":\"s\",\"issue\":null}\n"
		if err := os.WriteFile(filepath.Join(root, day.Format("2006-01-02")+".jsonl"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rows, _, err := ReadRows(root, start, end)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].RowKey != start.Format("2006-01-02") {
		t.Fatalf("rows in 30-day window = %v, want %s through %s", rows, start.Format("2006-01-02"), end.Format("2006-01-02"))
	}
	var stderr bytes.Buffer
	if err := os.WriteFile(filepath.Join(root, "linear-token"), []byte("test-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	getenv := func(key string) string {
		if key == "LINEAR_API_KEY_FILE" {
			return filepath.Join(root, "linear-token")
		}
		return ""
	}
	args := []string{"--dir", root, "--repos", "owner/one", "--issue-prefix", "EX"}
	err = runOutcomesWithClock(t.Context(), args, &stderr, getenv, func() time.Time { return date })
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "outcomes", utcDate.Format("2006-01-02")+".jsonl")
	lines, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("default outcome file %q: %v", path, err)
	}
	if len(lines) != 0 {
		t.Fatalf("outcome lines = %q, want empty output for untagged rows", lines)
	}
}

func TestRunOutcomesRequiresLinearAccessToken(t *testing.T) {
	var stderr bytes.Buffer
	args := []string{"--dir", t.TempDir(), "--repos", "owner/one", "--issue-prefix", "EX"}
	err := runOutcomesWithClock(t.Context(), args, &stderr, func(string) string { return "" }, func() time.Time {
		return time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	})
	if err == nil || !strings.Contains(err.Error(), "linear access token") {
		t.Fatalf("runOutcomesWithClock() error = %v, want missing linear access token", err)
	}
}

func TestRequiredFlagsAreRejected(t *testing.T) {
	root := t.TempDir()
	getenv := func(string) string { return "" }
	now := func() time.Time { return time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC) }
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "report without dir", args: []string{"report", "--from", "2026-11-01", "--to", "2026-11-01", "--by", "issue"}, wantErr: "--dir is required"},
		{name: "drivers without dir", args: []string{"drivers", "--from", "2026-11-01", "--to", "2026-11-01"}, wantErr: "--dir is required"},
		{name: "outcomes without dir", args: []string{"outcomes", "--repos", "owner/one", "--issue-prefix", "EX"}, wantErr: "--dir is required"},
		{name: "outcomes without repos", args: []string{"outcomes", "--dir", root, "--issue-prefix", "EX"}, wantErr: "--repos is required"},
		{name: "outcomes without issue prefix", args: []string{"outcomes", "--dir", root, "--repos", "owner/one"}, wantErr: "--issue-prefix is required"},
		{name: "outcomes with lowercase issue prefix", args: []string{"outcomes", "--dir", root, "--repos", "owner/one", "--issue-prefix", "ex"}, wantErr: "invalid --issue-prefix"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := runWithClock(t.Context(), tc.args, &stdout, &stderr, getenv, now)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("runWithClock(%v) error = %v, want containing %q", tc.args, err, tc.wantErr)
			}
		})
	}
}
