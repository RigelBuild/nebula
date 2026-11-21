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

	"github.com/RigelBuild/nebula/tools/nemo-move/internal/layout"
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
	if err := VerifyAsUser(t.Context(), root, testLayout, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := VerifyAsUser(t.Context(), root, testLayout, bytes.NewReader(data)); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestVerifyGoogleTokens(t *testing.T) {
	root := t.TempDir()
	for _, account := range testLayout.Google.Accounts {
		dir := filepath.Join(root, testLayout.GoogleRoot(), account, "credentials")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "token.json"), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := verifyGoogleTokens(root, testLayout); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, testLayout.GoogleRoot(), testLayout.Google.Accounts[1], "credentials", "token.json"), []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyGoogleTokens(root, testLayout); err == nil {
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
	lay := layout.Layout{Rows: []layout.Row{{Path: "example/repos", Kind: layout.Copy, Dest: "/opt/data/nemo/repos"}}}
	if err := verifyAsRuntimeUser(t.Context(), root, Manifest{Roots: []string{dataRoot}}, lay, run); err == nil || !called {
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

func TestVerifyJujutsuRepositoriesWalksEveryCopyDest(t *testing.T) {
	lay, err := layout.Load(filepath.Join("..", "..", "testdata", "inventory.cue"))
	if err != nil {
		t.Fatal(err)
	}
	lay.Google.Accounts = nil
	root := t.TempDir()
	want := map[string]bool{}
	for _, row := range lay.Rows {
		if row.Kind != layout.Copy {
			continue
		}
		repo := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(row.Dest, "/")), "repo")
		if err := os.MkdirAll(filepath.Join(repo, ".jj"), 0o755); err != nil {
			t.Fatal(err)
		}
		want[repo] = false
	}
	run := func(_ context.Context, name string, args ...string) (string, error) {
		if name == "jj" && len(args) == 3 && args[0] == "status" && args[1] == "--repository" {
			if _, ok := want[args[2]]; ok {
				want[args[2]] = true
			}
		}
		return "", nil
	}
	if err := verifyAsRuntimeUser(t.Context(), root, Manifest{Roots: []string{dataRoot}}, lay, run); err != nil {
		t.Fatal(err)
	}
	for repo, seen := range want {
		if !seen {
			t.Fatalf("jj status did not run in %s", repo)
		}
	}
}

func TestVerifyJujutsuRepositoriesSkipsLostFound(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads a mode 000 directory; the regression needs the runtime user")
	}
	lay := layout.Layout{Rows: []layout.Row{
		{Path: "example", Kind: layout.Copy, Dest: "/opt/data"},
		{Path: "example/reports", Kind: layout.Copy, Dest: "/opt/data/example/reports"},
	}}
	root := t.TempDir()
	repo := filepath.Join(root, "opt/data/example/reports/repo")
	if err := os.MkdirAll(filepath.Join(repo, ".jj"), 0o755); err != nil {
		t.Fatal(err)
	}
	lost := filepath.Join(root, "opt/data/lost+found")
	if err := os.Mkdir(lost, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(lost, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(lost, 0o700); err != nil {
			t.Error(err)
		}
	})
	calls := map[string]int{}
	run := func(_ context.Context, name string, args ...string) (string, error) {
		if name == "jj" && len(args) == 3 && args[0] == "status" && args[1] == "--repository" {
			calls[args[2]]++
		}
		return "", nil
	}
	if err := verifyAsRuntimeUser(t.Context(), root, Manifest{Roots: []string{dataRoot}}, lay, run); err != nil {
		t.Fatal(err)
	}
	if calls[repo] != 1 {
		t.Fatalf("jj status ran %d times in %s, want 1", calls[repo], repo)
	}
}
