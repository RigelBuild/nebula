package macside

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func testHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, row := range Rows() {
		if row.Kind == Copy && !filepath.IsAbs(row.Path) {
			if err := os.MkdirAll(filepath.Join(home, row.Path), 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeTestFile(t, home, ".hermes/state.db", "state")
	writeTestFile(t, home, ".local/state/nemo-move-staging/workspace/credentials/token", "token")
	writeTestFile(t, home, ".local/state/nemo-move-staging/personal/credentials/token", "token")
	writeTestFile(t, home, ".local/state/nemo-move/last-quiet", "quiet")
	quiet := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(home, ".local/state/nemo-move/last-quiet"), quiet, quiet); err != nil {
		t.Fatal(err)
	}
	return home
}

func writeTestFile(t *testing.T, home, path, content string) {
	t.Helper()
	full := filepath.Join(home, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestInventoryMissingCopy(t *testing.T) {
	home := testHome(t)
	if err := os.RemoveAll(filepath.Join(home, "nemo")); err != nil {
		t.Fatal(err)
	}
	if _, err := Inventory(home, time.Now()); err == nil {
		t.Fatal("missing copy row accepted")
	}
}

func TestInventoryUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read mode 000 files")
	}
	home := testHome(t)
	if err := os.Chmod(filepath.Join(home, ".hermes/state.db"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := Inventory(home, time.Now()); err == nil {
		t.Fatal("unreadable file accepted")
	}
}

func TestInventoryAbsoluteSymlinkOutsideHome(t *testing.T) {
	home := testHome(t)
	if err := os.Symlink("/outside", filepath.Join(home, ".hermes/link")); err != nil {
		t.Fatal(err)
	}
	if _, err := Inventory(home, time.Now()); err == nil {
		t.Fatal("outside symlink accepted")
	}
}

func TestInventorySizeCap(t *testing.T) {
	home := testHome(t)
	previous := maxBytes
	maxBytes = 1
	t.Cleanup(func() { maxBytes = previous })
	if _, err := Inventory(home, time.Now()); err == nil {
		t.Fatal("oversize inventory accepted")
	}
}

func TestInventoryAcceptsBinaryContainingSourceHome(t *testing.T) {
	for _, tc := range []struct {
		name, content string
	}{
		{"sqlite", "SQLite format 3\x00/Users/example/file"},
		{"nul", "binary\x00/Users/example/file"},
		{"invalid utf8", "\xff/Users/example/file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := testHome(t)
			path := ".hermes/state.db"
			writeTestFile(t, home, path, tc.content)
			if _, err := Inventory(home, time.Now()); err != nil {
				t.Fatalf("inventory error = %v", err)
			}
		})
	}
}

func TestInventoryAcceptsTextAndBinaryWithoutSourceHome(t *testing.T) {
	for _, tc := range []struct{ name, content string }{
		{"text path", "cd /Users/example/.hermes"},
		{"binary no path", "SQLite format 3\x00data"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := testHome(t)
			writeTestFile(t, home, ".hermes/state.db", tc.content)
			if _, err := Inventory(home, time.Now()); err != nil {
				t.Fatalf("inventory: %v", err)
			}
		})
	}
}

func TestExportFreshManifestRoundTripAndRelativeSymlink(t *testing.T) {
	home := testHome(t)
	if err := os.Symlink("state.db", filepath.Join(home, ".hermes/link")); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	manifest, err := Export(home, filepath.Join(home, "missing-journals"), &archive)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Manifest
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(manifest, decoded) {
		t.Fatalf("manifest round trip changed: %#v", decoded)
	}
	reader := tar.NewReader(&archive)
	found := false
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "opt/data/link" && header.Typeflag == tar.TypeSymlink && header.Linkname == "state.db" {
			found = true
		}
	}
	if !found {
		t.Fatal("relative symlink missing from archive")
	}
}

func TestExportJournals(t *testing.T) {
	home := testHome(t)
	dir := t.TempDir()
	writeTestFile(t, dir, "pages.jsonl", "journal\n")
	writeTestFile(t, dir, "ignored.txt", "ignored")
	var archive bytes.Buffer
	manifest, err := Export(home, dir, &archive)
	if err != nil {
		t.Fatal(err)
	}
	contents := map[string]string{}
	reader := tar.NewReader(&archive)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		contents[header.Name] = string(data)
	}
	if contents["control/nemo-page/pages.jsonl"] != "journal\n" {
		t.Fatal("journal missing from archive")
	}
	if _, ok := contents["control/nemo-page/ignored.txt"]; ok {
		t.Fatal("non-journal file included")
	}
	found := false
	for _, entry := range manifest.Entries {
		if entry.Path == "control/nemo-page/pages.jsonl" && entry.Type == "file" && entry.Size == 8 && len(entry.SHA256) == 64 {
			found = true
		}
	}
	if !found {
		t.Fatal("journal missing from manifest")
	}
}

func TestExportAcceptsBinaryJournalContainingSourceHome(t *testing.T) {
	for _, content := range []string{"SQLite format 3\x00/Users/example/a", "\xff/Users/example/a", "\x00/Users/example/a"} {
		home := testHome(t)
		dir := t.TempDir()
		writeTestFile(t, dir, "pages.jsonl", content)
		var output bytes.Buffer
		if _, err := Export(home, dir, &output); err != nil {
			t.Fatalf("export error = %v", err)
		}
	}
}

func TestExportMissingJournalDirectory(t *testing.T) {
	home := testHome(t)
	var archive bytes.Buffer
	manifest, err := Export(home, filepath.Join(home, "missing-journals"), &archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range manifest.Roots {
		if root == "control/nemo-page" {
			t.Fatal("missing journal directory included in manifest")
		}
	}
}

type touchedWriter struct{ touched bool }

func (w *touchedWriter) Write(p []byte) (int, error) {
	w.touched = true
	return len(p), nil
}

func TestExportStaleWriterUntouched(t *testing.T) {
	home := testHome(t)
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(home, ".local/state/nemo-move/last-quiet"), future, future); err != nil {
		t.Fatal(err)
	}
	writer := &touchedWriter{}
	if _, err := Export(home, filepath.Join(home, "missing-journals"), writer); err == nil {
		t.Fatal("stale staging accepted")
	}
	if writer.touched {
		t.Fatal("writer touched on stale staging")
	}
}

func TestInventoryReportsAllRows(t *testing.T) {
	home := testHome(t)
	writeTestFile(t, home, "nemo/unlisted/file", "data")
	writeTestFile(t, home, "nemo/repos/Other/project/file", "data")
	if err := os.RemoveAll(filepath.Join(home, "nemo/health")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(home, ".local/state/nemo-move-staging/personal")); err != nil {
		t.Fatal(err)
	}
	report, err := Inventory(home, time.Now())
	if err == nil {
		t.Fatal("missing rows accepted")
	}
	found := map[string]bool{}
	for _, item := range report.Rows {
		if item.Row.Path == "nemo/unlisted" && item.Row.Kind == "Copy (unknown)" && item.Readable {
			found["unknown"] = true
		}
		if item.Row.Path == "nemo/repos/Other" && item.Row.Kind == copyUnknown && item.Readable {
			found["owner"] = true
		}
		if item.Row.Path == "nemo/health" && item.Error != "" {
			found["missing"] = true
		}
		if item.Row.Path == ".local/state/nemo-move-staging/personal/credentials" && item.Error != "" {
			found["staging"] = true
		}
		if item.Row.Kind == Retire {
			found["retire"] = true
		}
		if item.Row.Kind == Rebuild {
			found["rebuild"] = true
		}
	}
	for _, key := range []string{"unknown", "owner", "missing", "staging", "retire", "rebuild"} {
		if !found[key] {
			t.Errorf("missing report entry %s", key)
		}
	}
}

func TestExportCheckedRejectsChangedFile(t *testing.T) {
	home := testHome(t)
	journalDir := filepath.Join(home, "missing-journals")
	manifest, err := Export(home, journalDir, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, home, ".hermes/state.db", "other")
	if _, err := ExportChecked(home, journalDir, io.Discard, manifest); err == nil {
		t.Fatal("changed file accepted")
	}
}

func TestInventoryReportsUnknownChildFailure(t *testing.T) {
	home := testHome(t)
	if err := os.Symlink("/outside", filepath.Join(home, "nemo/unlisted")); err != nil {
		t.Fatal(err)
	}
	report, err := Inventory(home, time.Now())
	if err == nil {
		t.Fatal("invalid unknown child accepted")
	}
	for _, item := range report.Rows {
		if item.Row.Path == "nemo/unlisted" && item.Row.Kind == "Copy (unknown)" && item.Error != "" {
			return
		}
	}
	t.Fatal("unknown child failure missing from report")
}
