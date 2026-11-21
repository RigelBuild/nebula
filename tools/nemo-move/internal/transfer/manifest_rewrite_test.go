package transfer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestRewriteFileAllowlist(t *testing.T) {
	for _, tc := range []struct{ name, path, input, want string }{
		{"repo text", "opt/data/nemo/repos/repo/file", testSourceHome + "/file", testSourceHome + "/file"},
		{"repo binary", "opt/data/nemo/repos/repo/.git/objects/pack", "\x00" + testSourceHome + "/file", "\x00" + testSourceHome + "/file"},
		{"config", "opt/data/config.yaml", testSourceHome + "/.hermes/file", "/opt/data/file"},
		{"boundary", "opt/data/.env", testSourceHome + "X/file", testSourceHome + "X/file"},
		{"script", "opt/data/scripts/run", testSourceHome + "/file", "/opt/data/file"},
		{"cron", "opt/data/cron/job", testSourceHome + "/file", "/opt/data/file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sum := sha256.Sum256([]byte(tc.input))
			entry := ManifestEntry{Path: tc.path, Size: int64(len(tc.input)), SHA256: hex.EncodeToString(sum[:])}
			f, expected, err := rewriteFile(bytes.NewBufferString(tc.input), entry, rewriteFilePath(tc.path), t.TempDir(), testSourceHome)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			got, err := io.ReadAll(f)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
			sum = sha256.Sum256(got)
			if expected.Size != int64(len(got)) || expected.SHA256 != hex.EncodeToString(sum[:]) {
				t.Fatalf("incorrect destination digest: %+v", expected)
			}
		})
	}
}

func TestScanReservedDirectories(t *testing.T) {
	for _, tc := range []struct{ name, path string }{{"scratch", rewriteTempRoot}, {"lost+found", dataRoot + "/lost+found"}} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, tc.path)
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if tc.name == "lost+found" && os.Getuid() != 0 {
				t.Skip("lost+found must be root-owned")
			}
			if err := os.WriteFile(filepath.Join(dir, "ignored"), []byte("x"), 0600); err != nil {
				t.Fatal(err)
			}
			m, err := scanTree(root, []string{dataRoot})
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range m.Entries {
				if e.Path == tc.path || e.Path == tc.path+"/ignored" {
					t.Fatalf("reserved entry: %s", e.Path)
				}
			}
		})
	}
}

func TestScanRefusesLostFoundFile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, dataRoot)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lost+found"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := scanTree(root, []string{dataRoot})
	if err == nil || err.Error() != "lost+found must be a root-owned directory" {
		t.Fatalf("scan error = %v", err)
	}
}
