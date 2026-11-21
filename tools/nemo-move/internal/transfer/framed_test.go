package transfer

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestFramedArchiveRejectsTampering(t *testing.T) {
	var source bytes.Buffer
	hash := sha256.Sum256([]byte("good"))
	manifest := Manifest{Roots: []string{"opt/data"}, Entries: []ManifestEntry{{Path: "opt/data/file", Type: "file", Mode: 0o600, Size: 4, SHA256: hex.EncodeToString(hash[:])}}}
	encoder := json.NewEncoder(&source)
	if err := encoder.Encode(struct {
		Roots []string `json:"roots"`
	}{manifest.Roots}); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Encode(manifest.Entries[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := source.WriteString("null\n"); err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(&source)
	if err := tw.WriteHeader(&tar.Header{Name: "opt/data/file", Mode: 0o600, Size: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("evil")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := writeFramedArchive(&source, &output); err == nil || output.Len() != 0 {
		t.Fatalf("tampered archive: error %v, output %d", err, output.Len())
	}
}

func TestFramedArchivePreservesSourceSymlinkManifest(t *testing.T) {
	var source bytes.Buffer
	target := "/Users/example/.hermes/state.db"
	entry := ManifestEntry{Path: "opt/data/link", Type: symlinkType, Mode: 0o777, Size: int64(len(target)), Target: target}
	encoder := json.NewEncoder(&source)
	if err := encoder.Encode(Manifest{Roots: []string{dataRoot}}); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Encode(entry); err != nil {
		t.Fatal(err)
	}
	if _, err := source.WriteString("null\n"); err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(&source)
	if err := tw.WriteHeader(&tar.Header{Name: entry.Path, Typeflag: tar.TypeSymlink, Mode: 0o777, Linkname: target}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := writeFramedArchive(&source, &output); err != nil {
		t.Fatal(err)
	}
	manifest, err := ReadArchiveManifest(&output)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Entries[0].Target != target {
		t.Fatalf("source target = %q, want %q", manifest.Entries[0].Target, target)
	}
}

func TestFramedExtractPreservesSourceAndVerifiesRewrittenManifest(t *testing.T) {
	content := []byte("cd /Users/example/.hermes\nfile=/Users/example/nemo/x\n")
	hash := sha256.Sum256(content)
	target := "/Users/example/.hermes/file"
	entries := []ManifestEntry{
		{Path: "opt/data", Type: directoryType, Mode: 0o700},
		{Path: "opt/data/config.yaml", Type: fileType, Mode: 0o600, Size: int64(len(content)), SHA256: hex.EncodeToString(hash[:])},
		{Path: "opt/data/link", Type: symlinkType, Mode: 0o777, Size: int64(len(target)), Target: target},
	}
	var source bytes.Buffer
	enc := json.NewEncoder(&source)
	if err := enc.Encode(Manifest{Roots: []string{dataRoot}}); err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if err := enc.Encode(entry); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := io.WriteString(&source, "null\n"); err != nil {
		t.Fatal(err)
	}
	w := tar.NewWriter(&source)
	for _, header := range []*tar.Header{
		{Name: entries[0].Path, Typeflag: tar.TypeDir, Mode: 0o700},
		{Name: entries[1].Path, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(content))},
		{Name: entries[2].Path, Typeflag: tar.TypeSymlink, Mode: 0o777, Linkname: target},
	} {
		if err := w.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := w.Write(content); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := writeFramedArchive(&source, &archive); err != nil {
		t.Fatal(err)
	}
	sourceManifest, err := ReadArchiveManifest(bytes.NewReader(archive.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if sourceManifest.Entries[1] != entries[1] || sourceManifest.Entries[2] != entries[2] {
		t.Fatalf("source manifest changed: %+v", sourceManifest)
	}
	root := t.TempDir()
	dest, err := ExtractForTest(&archive, root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "opt/data/config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "cd /opt/data\nfile=/opt/data/nemo/x\n" {
		t.Fatalf("rewritten file = %q", got)
	}
	link, err := os.Readlink(filepath.Join(root, "opt/data/link"))
	if err != nil || link != "/opt/data/file" {
		t.Fatalf("link = %q, %v", link, err)
	}
	if dest.Entries[1].SHA256 == entries[1].SHA256 || dest.Entries[2].Target != link {
		t.Fatalf("destination manifest not rewritten: %+v", dest)
	}
}

func TestRewriteLink(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{input: "/Users/example/.hermes/state.db", want: "/opt/data/state.db"},
		{input: "/Users/example/nemo/file", want: "/opt/data/nemo/file"},
		{input: "../relative", want: "../relative"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, err := rewriteLink(tc.input, testSourceHome)
			if err != nil || got != tc.want {
				t.Fatalf("rewriteLink = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestFramedArchiveWaitsForRemoteSuccess(t *testing.T) {
	var source bytes.Buffer
	encoder := json.NewEncoder(&source)
	if err := encoder.Encode(struct {
		Roots []string `json:"roots"`
	}{[]string{"opt/data"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := source.WriteString("null\n"); err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(&source)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := writeFramedArchiveTo(&source, &output, t.TempDir(), func() error { return errors.New("remote failed") })
	if err == nil || output.Len() != 0 {
		t.Fatalf("remote failure: error %v, output %d", err, output.Len())
	}
}

func TestFramedArchiveRejectsInvalidManifestLines(t *testing.T) {
	for _, tc := range []struct{ name, input string }{
		{"traversal", "{\"roots\":[\"opt/data\"]}\n{\"path\":\"opt/data/../../etc\",\"type\":\"file\"}\nnull\n"},
		{"type", "{\"roots\":[\"opt/data\"]}\n{\"path\":\"opt/data/x\",\"type\":\"other\"}\nnull\n"},
		{"unknown root field", "{\"roots\":[\"opt/data\"],\"extra\":1}\n"},
		{"unknown entry field", "{\"roots\":[\"opt/data\"]}\n{\"path\":\"opt/data/x\",\"type\":\"directory\",\"extra\":1}\nnull\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := writeFramedArchive(bytes.NewBufferString(tc.input), &output); err == nil {
				t.Fatal("invalid manifest accepted")
			}
			if output.Len() != 0 {
				t.Fatal("invalid archive emitted")
			}
		})
	}
}
