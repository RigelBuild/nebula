package client

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/inventory"
)

// stubRestic answers `cat config` as an existing repo and `backup` with a snapshot id.
func stubRestic(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "restic")
	body := "#!/bin/sh\nfor a in \"$@\"; do\n  if [ \"$a\" = backup ]; then echo '{\"message_type\":\"summary\",\"snapshot_id\":\"stubsnap\"}'; exit 0; fi\ndone\nexit 0\n"
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPassAcceptsBackendRepository(t *testing.T) {
	for _, repo := range []string{"s3:https://acct.example.invalid/bucket/host", "rest:http://127.0.0.1:1/x"} {
		c, _, _ := openTestClient(t, repo, WithRestic(stubRestic(t)))
		path := filepath.Join(t.TempDir(), "session.jsonl")
		scanLines(t, c, writeVersion(t, path, "one", "normal"))
		if outcome, err := c.Pass(t.Context(), []string{path}, nil); err != nil || outcome.Kind != Acked {
			t.Fatalf("%s: pass = %#v, %v", repo, outcome, err)
		}
	}
}

func TestPassStartLineBounded(t *testing.T) {
	c, _, state := openTestClient(t, "s3:https://acct.example.invalid/bucket/host", WithRestic(stubRestic(t)))
	dir := t.TempDir()
	const total = 50000
	files := make([]string, total)
	lines := make([]inventory.Line, total)
	for i := range files {
		files[i] = filepath.Join(dir, fmt.Sprintf("f%05d", i))
		if err := os.WriteFile(files[i], nil, 0o600); err != nil {
			t.Fatal(err)
		}
		lines[i] = inventory.Line{Path: files[i], Type: "file", Class: "final-only"}
	}
	pending := writeVersion(t, filepath.Join(dir, "pending.jsonl"), "data", "normal")
	lines = append(lines, pending)
	scanLines(t, c, lines...)
	if outcome, err := c.Pass(t.Context(), append(files, pending.Path), nil); err != nil || outcome.Kind != Acked {
		t.Fatalf("pass = %#v, %v", outcome, err)
	}
	if oldest, size := c.Pending(); !oldest.IsZero() || size != 0 {
		t.Fatalf("pending after ack %v/%d", oldest, size)
	}
	journal, err := os.Open(filepath.Join(state, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	scanner := bufio.NewScanner(journal)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	found := false
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), `"event":"pass-start"`) {
			found = true
			if len(scanner.Bytes()) > 16*1024 {
				t.Fatalf("pass-start line is %d bytes for %d files", len(scanner.Bytes()), total+1)
			}
		}
	}
	if !found {
		t.Fatal("no pass-start event")
	}
	reopened, err := Open(state, RepoRef{Scope: "k3s-test-one", Repository: c.repo.Repository}, &fakeEscrow{}, WithScratch(t.TempDir()), WithRestic(c.restic))
	if err != nil {
		t.Fatal(err)
	}
	if oldest, _ := reopened.Pending(); !oldest.IsZero() {
		t.Fatal("pending path not acked after journal replay")
	}
}
