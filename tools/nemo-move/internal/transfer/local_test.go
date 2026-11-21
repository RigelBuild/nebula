package transfer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestRuntimeCredential(t *testing.T) {
	uid, gid, err := runtimeCredential("10000")
	if err != nil || uid != 10000 || gid != 10000 {
		t.Fatalf("numeric credential = %d:%d, %v", uid, gid, err)
	}
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	uid, gid, err = runtimeCredential(account.Username)
	if err != nil || strconv.FormatUint(uint64(uid), 10) != account.Uid || strconv.FormatUint(uint64(gid), 10) != account.Gid {
		t.Fatalf("named credential = %d:%d, %v", uid, gid, err)
	}
}

func TestVerifyAsUser(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "control/nemo-page")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "test.jsonl")
	if err := os.WriteFile(file, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{Roots: []string{pageRoot}, Entries: []ManifestEntry{
		{Path: pageRoot, Type: directoryType, Mode: 0o755},
		{Path: pageRoot + "/test.jsonl", Type: fileType, Mode: 0o600, SHA256: strings.Repeat("0", 64)},
	}}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAsUser(t.Context(), root, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := VerifyAsUser(t.Context(), root, bytes.NewReader(data)); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestVerifyGoogleTokens(t *testing.T) {
	root := t.TempDir()
	for _, account := range []string{"workspace", "personal"} {
		dir := filepath.Join(root, "opt/data/.local/state/nemo-google-mcp", account, "credentials")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "token.json"), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := verifyGoogleTokens(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "opt/data/.local/state/nemo-google-mcp/personal/credentials/token.json"), []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyGoogleTokens(root); err == nil {
		t.Fatal("invalid token accepted")
	}
}

func TestVerifyRuntimeLinksAndRepositories(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "opt/data/nemo/repos/repo")
	if err := os.MkdirAll(filepath.Join(dir, ".jj"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "opt/data/link")
	if err := os.Symlink("nemo/repos", link); err != nil {
		t.Fatal(err)
	}
	if err := verifyLink(root, link, []string{dataRoot}); err != nil {
		t.Fatal(err)
	}
	called := false
	run := func(_ context.Context, name string, args ...string) (string, error) {
		called = name == "jj" && len(args) == 3 && args[0] == "status" && args[1] == "--repository" && args[2] == dir
		return "", errors.New("status failed")
	}
	if err := verifyJujutsuRepositories(t.Context(), filepath.Join(root, "opt/data/nemo/repos"), run); err == nil || !called {
		t.Fatalf("jj status: called = %v, error = %v", called, err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/opt/data/nemo/repos", link); err != nil {
		t.Fatal(err)
	}
	if err := verifyLink(root, link, []string{dataRoot}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../etc", link); err != nil {
		t.Fatal(err)
	}
	if err := verifyLink(root, link, []string{dataRoot}); err == nil {
		t.Fatal("escaping symlink accepted")
	}

	chain := filepath.Join(root, "opt/data/a/b")
	if err := os.MkdirAll(chain, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/opt/data", filepath.Join(chain, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sub/../../..", filepath.Join(chain, "link")); err != nil {
		t.Fatal(err)
	}
	if err := verifyLink(root, filepath.Join(chain, "link"), []string{dataRoot}); err == nil {
		t.Fatal("escaping symlink chain accepted")
	}
}
