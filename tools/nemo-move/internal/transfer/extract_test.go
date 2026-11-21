package transfer

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func ExtractForTest(source *bytes.Buffer, root string) (Manifest, error) {
	restore := useTestJournalLocker()
	defer restore()
	return extract(source, root, testOptions, func(string, Manifest) error { return nil }, func(root string, want Manifest) error {
		got, err := scanTree(root, want.Roots)
		if err != nil {
			return err
		}
		got.Entries = slices.DeleteFunc(got.Entries, func(entry ManifestEntry) bool {
			return entry.Path == pageRoot+"/.initialized"
		})
		if !reflect.DeepEqual(got, want) {
			return fmt.Errorf("manifest mismatch: got %+v, want %+v", got, want)
		}
		return nil
	})
}

// FrameForTest runs the same framing pass used by Export.
func FrameForTest(source, output *bytes.Buffer) error { return writeFramedArchive(source, output) }

func extractionArchive(t *testing.T, manifest Manifest, headers ...*tar.Header) *bytes.Buffer {
	t.Helper()
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	w := tar.NewWriter(&out)
	if err := w.WriteHeader(&tar.Header{Name: manifestName, Typeflag: tar.TypeReg, Mode: 0o444, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	for _, h := range headers {
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := w.Write(make([]byte, h.Size)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &out
}

func TestExtractPartialThenRerun(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(useTestJournalLocker())
	hash := sha256.Sum256(nil)
	rootEntry := ManifestEntry{Path: dataRoot, Type: directoryType, Mode: 0o700}
	dir := ManifestEntry{Path: "opt/data/nemo", Type: directoryType, Mode: 0o700}
	file := ManifestEntry{Path: "opt/data/nemo/file", Type: fileType, Mode: 0o600, SHA256: hex.EncodeToString(hash[:])}
	page := ManifestEntry{Path: pageRoot, Type: directoryType, Mode: 0o755}
	journal := ManifestEntry{Path: pageRoot + "/pages.jsonl", Type: fileType, Mode: 0o600, SHA256: hex.EncodeToString(hash[:])}
	manifest := Manifest{Roots: []string{pageRoot, dataRoot}, Entries: []ManifestEntry{page, journal, rootEntry, dir, file}}
	rootHeader := &tar.Header{Name: dataRoot, Typeflag: tar.TypeDir, Mode: 0o700}
	dirHeader := &tar.Header{Name: dir.Path, Typeflag: tar.TypeDir, Mode: 0o700}
	fileHeader := &tar.Header{Name: file.Path, Typeflag: tar.TypeReg, Mode: 0o600}
	pageHeader := &tar.Header{Name: pageRoot, Typeflag: tar.TypeDir, Mode: 0o755}
	journalHeader := &tar.Header{Name: journal.Path, Typeflag: tar.TypeReg, Mode: 0o600}
	partial := extractionArchive(t, manifest, rootHeader, dirHeader, pageHeader, journalHeader)
	if _, err := extract(partial, root, testOptions, func(string, Manifest) error { return nil }, func(string, Manifest) error { return nil }); err == nil {
		t.Fatal("partial archive accepted")
	}
	if _, err := os.Stat(filepath.Join(root, rewriteTempRoot)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scratch left after error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, journal.Path+".incoming")); err != nil {
		t.Fatalf("partial journal missing: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, file.Path), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	full := extractionArchive(t, manifest, rootHeader, dirHeader, fileHeader, pageHeader, journalHeader)
	if _, err := ExtractForTest(full, root); err != nil {
		t.Fatalf("rerun verification: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, rewriteTempRoot)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scratch left after success: %v", err)
	}
}

func TestExtractEmptyJournalManifestRerun(t *testing.T) {
	for _, tc := range []struct {
		name        string
		initialized bool
	}{
		{name: "first initialization"},
		{name: "rerun", initialized: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, pageRoot)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.initialized {
				if err := os.WriteFile(filepath.Join(dir, ".initialized"), nil, 0o444); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "pages.jsonl"), []byte("existing"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(useTestJournalLocker())
			page := ManifestEntry{Path: pageRoot, Type: directoryType, Mode: 0o755}
			archive := extractionArchive(t, Manifest{Roots: []string{pageRoot}, Entries: []ManifestEntry{page}}, &tar.Header{Name: pageRoot, Typeflag: tar.TypeDir, Mode: 0o755})
			got, err := ExtractForTest(archive, root)
			if err != nil {
				t.Fatal(err)
			}
			want := 3
			if tc.initialized {
				want = 2
			}
			if len(got.Entries) != want {
				t.Fatalf("manifest entries = %+v", got.Entries)
			}
		})
	}
}

func TestExtractCleanupDoesNotFollowSymlinks(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "opt/data/nemo"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "opt/data/nemo/link")); err != nil {
		t.Fatal(err)
	}
	dir := ManifestEntry{Path: "opt/data/nemo", Type: directoryType, Mode: 0o700}
	manifest := Manifest{Roots: []string{dataRoot}, Entries: []ManifestEntry{dir}}
	archive := extractionArchive(t, manifest, &tar.Header{Name: dir.Path, Typeflag: tar.TypeDir, Mode: 0o700})
	t.Cleanup(useTestJournalLocker())
	if _, err := extract(archive, root, testOptions, func(string, Manifest) error { return nil }, func(string, Manifest) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(outside, "keep")); err != nil || string(data) != "safe" {
		t.Fatalf("outside file = %q, %v", data, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "opt/data/nemo/link")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("link remains: %v", err)
	}
}

func TestExtractRejectsInvalidArchives(t *testing.T) {
	dir := ManifestEntry{Path: "opt/data", Type: directoryType, Mode: 0o700}
	file := ManifestEntry{Path: "opt/data/file", Type: fileType, Mode: 0o600, SHA256: strings.Repeat("0", 64)}
	dirHeader := &tar.Header{Name: dir.Path, Typeflag: tar.TypeDir, Mode: 0o700}
	fileHeader := &tar.Header{Name: file.Path, Typeflag: tar.TypeReg, Mode: 0o600}
	cases := []struct {
		name    string
		entries []ManifestEntry
		headers []*tar.Header
	}{
		{"parent traversal", []ManifestEntry{dir}, []*tar.Header{{Name: "../outside", Typeflag: tar.TypeReg}}},
		{"absolute name", []ManifestEntry{dir}, []*tar.Header{{Name: "/outside", Typeflag: tar.TypeReg}}},
		{"type mismatch", []ManifestEntry{dir, file}, []*tar.Header{dirHeader, {Name: file.Path, Typeflag: tar.TypeDir, Mode: 0o600}}},
		{"duplicate", []ManifestEntry{dir, file}, []*tar.Header{dirHeader, fileHeader, fileHeader}},
		{"missing", []ManifestEntry{dir, file}, []*tar.Header{dirHeader}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			archive := extractionArchive(t, Manifest{Roots: []string{dataRoot}, Entries: tc.entries}, tc.headers...)
			if _, err := extract(archive, t.TempDir(), testOptions, func(string, Manifest) error { return nil }, func(string, Manifest) error { return nil }); err == nil {
				t.Fatal("invalid archive accepted")
			}
		})
	}
}

func TestExtractReplacesExistingPagingJournal(t *testing.T) {
	root := t.TempDir()
	page := filepath.Join(root, pageRoot)
	if err := os.MkdirAll(page, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(page, "pages.jsonl"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := ManifestEntry{Path: pageRoot, Type: directoryType, Mode: 0o755}
	hash := sha256.Sum256(nil)
	file := ManifestEntry{Path: pageRoot + "/pages.jsonl", Type: fileType, Mode: 0o600, SHA256: hex.EncodeToString(hash[:])}
	archive := extractionArchive(t, Manifest{Roots: []string{pageRoot}, Entries: []ManifestEntry{dir, file}},
		&tar.Header{Name: dir.Path, Typeflag: tar.TypeDir, Mode: 0o755},
		&tar.Header{Name: file.Path, Typeflag: tar.TypeReg, Mode: 0o600})
	ownershipErr := fmt.Errorf("ownership refused")
	if _, err := extract(archive, root, testOptions, func(string, Manifest) error { return ownershipErr }, func(string, Manifest) error { t.Fatal("compared after ownership failure"); return nil }); !errors.Is(err, ownershipErr) {
		t.Fatalf("ownership failure = %v", err)
	}
	old, err := os.ReadFile(filepath.Join(page, "pages.jsonl"))
	if err != nil || string(old) != "old" {
		t.Fatalf("existing journal = %q, %v", old, err)
	}
	incoming, err := os.ReadFile(filepath.Join(page, "pages.jsonl.incoming"))
	if err != nil || len(incoming) != 0 {
		t.Fatalf("incoming journal = %q, %v", incoming, err)
	}
}

func TestExtractRewritesSourceHomeSymlink(t *testing.T) {
	t.Cleanup(useTestJournalLocker())
	for _, tc := range []struct {
		name, target, want string
		fail               bool
	}{
		{name: "source home", target: testSourceHome + "/.example-agent/file", want: "/opt/data/file"},
		{name: "target home", target: "/opt/data/file", want: "/opt/data/file"},
		{name: "outside homes", target: "/etc/passwd", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			entry := ManifestEntry{Path: dataRoot + "/link", Type: symlinkType, Mode: 0o777, Target: tc.target, Size: int64(len(tc.target))}
			archive := extractionArchive(t, Manifest{Roots: []string{dataRoot}, Entries: []ManifestEntry{entry}},
				&tar.Header{Name: entry.Path, Typeflag: tar.TypeSymlink, Mode: 0o777, Linkname: tc.target})
			manifest, err := extract(archive, root, testOptions, func(string, Manifest) error { return nil }, func(string, Manifest) error { return nil })
			if tc.fail {
				if err == nil {
					t.Fatal("outside symlink accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.Readlink(filepath.Join(root, entry.Path))
			if err != nil || got != tc.want {
				t.Fatalf("link = %q, %v; want %q", got, err, tc.want)
			}
			if manifest.Entries[0].Target != tc.want || manifest.Entries[0].Size != int64(len(tc.want)) {
				t.Fatalf("manifest = %+v", manifest.Entries[0])
			}
		})
	}
}

func TestExtractRejectsSymlinkParents(t *testing.T) {
	for _, dangling := range []bool{false, true} {
		t.Run(map[bool]string{false: "outside", true: "dangling"}[dangling], func(t *testing.T) {
			root := t.TempDir()
			outside := t.TempDir()
			if dangling {
				outside = filepath.Join(outside, "missing")
			}
			if err := os.MkdirAll(filepath.Join(root, "opt/data"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(root, "opt/data/link")); err != nil {
				t.Fatal(err)
			}
			entry := ManifestEntry{Path: "opt/data/link/file", Type: fileType, Mode: 0o600, SHA256: strings.Repeat("0", 64)}
			archive := extractionArchive(t, Manifest{Roots: []string{dataRoot}, Entries: []ManifestEntry{entry}}, &tar.Header{Name: entry.Path, Typeflag: tar.TypeReg, Mode: 0o600})
			if _, err := extract(archive, root, testOptions, func(string, Manifest) error { return nil }, func(string, Manifest) error { return nil }); err == nil {
				t.Fatal("symlink parent accepted")
			}
			if _, err := os.Stat(filepath.Join(outside, "file")); !os.IsNotExist(err) {
				t.Fatalf("outside file exists: %v", err)
			}
		})
	}
}

func TestOwnEntryTypesAndPagingMode(t *testing.T) {
	root := t.TempDir()
	page := filepath.Join(root, pageRoot)
	if err := os.MkdirAll(page, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(page, "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("file", filepath.Join(page, "link")); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	for _, entry := range []ManifestEntry{
		{Path: pageRoot + "/file", Type: symlinkType},
		{Path: pageRoot + "/link", Type: fileType},
	} {
		if err := ownEntry(fd, entry, os.Getuid(), os.Getgid()); err == nil {
			t.Errorf("accepted mismatched type for %s", entry.Path)
		}
	}
	if err := ownEntry(fd, ManifestEntry{Path: pageRoot, Type: directoryType}, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(page)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("paging mode = %o", info.Mode().Perm())
	}
}
