package retrieve_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/client"
	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/inventory"
	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/retrieve"
)

type escrow struct{}

func (escrow) Put(_ context.Context, _ string, body []byte) ([32]byte, error) {
	return sha256.Sum256(body), nil
}

func writeFile(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func wrapRestic(t *testing.T) string {
	t.Helper()
	real, err := exec.LookPath("restic")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := filepath.Join(t.TempDir(), "calls")
	script := fmt.Sprintf("#!/bin/sh\ncat /proc/$$/cmdline >> %q\ncat /proc/$$/environ >> %q\nprintf '\\n' >> %q\nexec %q \"$@\"\n", log, log, log, real)
	writeFile(t, filepath.Join(dir, "restic"), []byte(script))
	if err := os.Chmod(filepath.Join(dir, "restic"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func createEscrow(t *testing.T, password []byte) (string, string) {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	identityPath := filepath.Join(t.TempDir(), "identity")
	writeFile(t, identityPath, []byte(identity.String()+"\n"))
	var encrypted bytes.Buffer
	writer, err := age.Encrypt(&encrypted, identity.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(password); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	escrowPath := filepath.Join(t.TempDir(), "escrow.age")
	writeFile(t, escrowPath, encrypted.Bytes())
	return escrowPath, identityPath
}

func fixture(t *testing.T, root string) (map[string][]byte, string) {
	t.Helper()
	blob := []byte("fixture image bytes")
	digest := fmt.Sprintf("%x", sha256.Sum256(blob))
	files := map[string][]byte{
		"sessions/work/record.jsonl":      []byte(`{"blob":"blob:sha256:` + digest + `"}` + "\n"),
		"sessions/work/record/sub.jsonl":  []byte("nested\n"),
		"sessions/work/record/output.log": []byte("log\n"),
		"sessions/work/record/note.md":    []byte("markdown\n"),
		"blobs/" + digest:                 blob,
		"sessions/work/record/image.webp": blob,
	}
	for path, body := range files {
		if strings.HasSuffix(path, ".webp") {
			continue
		}
		writeFile(t, filepath.Join(root, path), body)
	}
	if err := os.Link(filepath.Join(root, "blobs", digest), filepath.Join(root, "sessions/work/record/image.webp")); err != nil {
		t.Fatal(err)
	}
	link := "sessions/work/record/link"
	if err := os.Symlink("note.md", filepath.Join(root, link)); err != nil {
		t.Fatal(err)
	}
	return files, link
}

func archive(t *testing.T, root, repo, state string, files map[string][]byte, link string) (client.Outcome, []byte) {
	t.Helper()
	c, err := client.Open(state, client.RepoRef{Scope: "k3s-test-one", Repository: repo}, escrow{}, client.WithScratch(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	password, err := os.ReadFile(filepath.Join(state, "password"))
	if err != nil {
		t.Fatal(err)
	}
	lines := make([]inventory.Line, 0, len(files)+1)
	paths := make([]string, 0, len(files)+1)
	for path, body := range files {
		absolute := filepath.Join(root, path)
		paths = append(paths, absolute)
		lines = append(lines, inventory.Line{Path: absolute, Type: "file", Size: int64(len(body)), SHA256: sha256.Sum256(body)})
	}
	linkPath := filepath.Join(root, link)
	paths = append(paths, linkPath)
	lines = append(lines, inventory.Line{Path: linkPath, Type: "symlink", LinkTarget: "note.md"})
	if err := c.Scan(t.Context(), inventory.Inventory{Lines: lines}, inventory.Delta{Added: lines}); err != nil {
		t.Fatal(err)
	}
	outcome, err := c.Pass(t.Context(), paths, nil)
	if err != nil || outcome.Kind != client.Acked {
		t.Fatalf("pass = %+v, %v", outcome, err)
	}
	return outcome, password
}

func TestGetRoundTripAndArgv(t *testing.T) {
	log := wrapRestic(t)
	root := filepath.Join(t.TempDir(), "unique-source-sentinel-8274")
	repo := filepath.Join(t.TempDir(), "repo")
	files, link := fixture(t, root)
	state := filepath.Join(t.TempDir(), "state")
	outcome, password := archive(t, root, repo, state, files, link)
	escrowPath, identity := createEscrow(t, password)
	output := filepath.Join(t.TempDir(), "out")
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
		t.Setenv(name, "http://127.0.0.1:1/")
	}
	t.Setenv("RESTIC_REPOSITORY", "rest:http://127.0.0.1:1/")
	if err := retrieve.Get(t.Context(), retrieve.Options{Repo: repo, Escrow: escrowPath, Identity: identity, Snapshot: outcome.SnapshotID, Record: "record", Output: output}); err != nil {
		t.Fatal(err)
	}
	for path, want := range files {
		got, err := os.ReadFile(filepath.Join(output, path))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("restored %s: %q, %v", path, got, err)
		}
	}
	if target, err := os.Readlink(filepath.Join(output, link)); err != nil || target != "note.md" {
		t.Errorf("symlink = %q, %v", target, err)
	}
	cmd := exec.CommandContext(t.Context(), "restic", "--repo", repo, "--password-file", filepath.Join(state, "password"), "snapshots")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("snapshots: %s: %v", output, err)
	}
	checkOfflineListings(t, repo, filepath.Join(state, "password"), outcome.SnapshotID)
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(calls, []byte("unique-source-sentinel-8274")) || bytes.Contains(calls, password) {
		t.Fatal("source sentinel or password appeared in restic argv or environment")
	}
}

// Restic restore lchown fails in user namespaces, so namespace checks use read-only commands.
func checkOfflineListings(t *testing.T, repo, password, snapshot string) {
	t.Helper()
	unshare, err := exec.LookPath("unshare")
	if err != nil {
		offlineUnavailable(t, err)
	}
	if err := exec.CommandContext(t.Context(), unshare, "-rn", "true").Run(); err != nil {
		offlineUnavailable(t, err)
	}
	for _, args := range [][]string{{"snapshots", "--json"}, {"ls", "--json", snapshot}} {
		commandArgs := append([]string{"-rn", "restic", "--repo", repo, "--password-file", password, "--no-lock"}, args...)
		cmd := exec.CommandContext(t.Context(), unshare, commandArgs...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("offline restic listing: %v: %s", err, output)
		}
	}
}

func offlineUnavailable(t *testing.T, err error) {
	t.Helper()
	if os.Getenv("CI") != "" {
		t.Fatalf("network namespace unavailable: %v", err)
	}
	t.Skipf("network namespace unavailable: %v", err)
}
