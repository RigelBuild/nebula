package client

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/inventory"
)

type fakeEscrow struct {
	body []byte
}

func (f *fakeEscrow) Put(_ context.Context, _ string, body []byte) ([32]byte, error) {
	f.body = append([]byte(nil), body...)
	return sha256.Sum256(body), nil
}

func openTestClient(t *testing.T, repo string, options ...Option) (*Client, *fakeEscrow, string) {
	t.Helper()
	root := t.TempDir()
	state := filepath.Join(root, "state")
	escrow := &fakeEscrow{}
	options = append([]Option{WithScratch(filepath.Join(root, "scratch"))}, options...)
	client, err := Open(state, RepoRef{Scope: "k3s-test-one", Repository: repo}, escrow, options...)
	if err != nil {
		t.Fatal(err)
	}
	return client, escrow, state
}

func localRestic(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("restic"); err != nil {
		t.Fatalf("restic must be on PATH: %v", err)
	}
	output, err := exec.Command("restic", "version").Output()
	if err != nil {
		t.Fatalf("read restic version: %v", err)
	}
	if !strings.Contains(string(output), "0.19.1") {
		t.Fatalf("restic version = %q, want pinned 0.19.1", output)
	}
}

func TestOpenCreatesEscrowedPassword(t *testing.T) {
	localRestic(t)
	repo := filepath.Join(t.TempDir(), "repo")
	client, escrow, state := openTestClient(t, repo)
	password, err := os.ReadFile(filepath.Join(state, "password"))
	if err != nil {
		t.Fatal(err)
	}
	if len(password) != 64 || regexp.MustCompile(`[^0-9a-f]`).Match(password) {
		t.Fatalf("password format = %q", password)
	}
	info, err := os.Stat(filepath.Join(state, "password"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("password mode = %o, want 600", info.Mode().Perm())
	}
	if len(escrow.body) != 0 {
		t.Fatal("Open called escrow")
	}
	if _, err := os.Stat(repo); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Open touched repository: %v", err)
	}
	if err := client.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	journal, err := os.ReadFile(filepath.Join(state, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	escrowIndex := strings.Index(string(journal), `"event":"escrowed"`)
	initIndex := strings.Index(string(journal), `"event":"init"`)
	if escrowIndex < 0 || initIndex <= escrowIndex {
		t.Fatalf("escrow and init journal order: %s", journal)
	}
	if string(escrow.body) != string(password) {
		t.Fatal("escrow received different password bytes")
	}
}

func TestPassAckChangedBytesAndEmptyDelta(t *testing.T) {
	localRestic(t)
	repo := filepath.Join(t.TempDir(), "repo")
	client, _, _ := openTestClient(t, repo)
	file := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(file, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	line := inventory.Line{Path: file, Type: "file", Size: 5, SHA256: sha256.Sum256([]byte("first"))}
	if err := client.Scan(t.Context(), inventory.Inventory{Lines: []inventory.Line{line}}, inventory.Delta{Added: []inventory.Line{line}}); err != nil {
		t.Fatal(err)
	}
	first, err := client.Pass(t.Context(), []string{file}, nil)
	if err != nil || first.Kind != Acked || first.SnapshotID == "" {
		t.Fatalf("first pass = %#v, %v", first, err)
	}
	if err := os.WriteFile(file, []byte("second version"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed := inventory.Line{Path: file, Type: "file", Size: 14, SHA256: sha256.Sum256([]byte("second version"))}
	if err := client.Scan(t.Context(), inventory.Inventory{Lines: []inventory.Line{changed}}, inventory.Delta{Changed: []inventory.Line{changed}}); err != nil {
		t.Fatal(err)
	}
	second, err := client.Pass(t.Context(), []string{file}, nil)
	if err != nil || second.Kind != Acked || second.SnapshotID == "" {
		t.Fatalf("second pass = %#v, %v", second, err)
	}
	if err := client.Scan(t.Context(), inventory.Inventory{Lines: []inventory.Line{changed}}, inventory.Delta{}); err != nil {
		t.Fatal(err)
	}
	oldest, bytes := client.Pending()
	if !oldest.IsZero() || bytes != 0 {
		t.Fatalf("pending after unchanged ack = %v/%d, want empty", oldest, bytes)
	}
}

func TestPassVanishedBeforePass(t *testing.T) {
	localRestic(t)
	client, _, state := openTestClient(t, filepath.Join(t.TempDir(), "repo"))
	missing := filepath.Join(t.TempDir(), "gone.jsonl")
	line := writeVersion(t, missing, "gone", "normal")
	scanLines(t, client, line)
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	resticWrapper(t, `exec REAL_RESTIC "$@" 2>/dev/null`)
	outcome, err := client.Pass(t.Context(), []string{missing}, nil)
	if err != nil || outcome.Kind != Acked || len(outcome.Vanished) != 1 || outcome.Vanished[0] != missing {
		t.Fatalf("pass = %#v, %v", outcome, err)
	}
	if !journalHas(t, state, "vanished", missing) {
		t.Fatal("journal missing vanished path")
	}
	if oldest, size := client.Pending(); !oldest.IsZero() || size != 0 {
		t.Fatalf("pending = %v/%d, want empty", oldest, size)
	}
}

func TestScanRevertClearsPending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	base := writeVersion(t, path, "base", "normal")
	baseline := inventory.Inventory{Lines: []inventory.Line{base}}
	client, _, _ := openTestClient(t, filepath.Join(t.TempDir(), "repo"), WithBaseline(&baseline), WithChained(true))
	changed := writeVersion(t, path, "other", "normal")
	if err := client.Scan(t.Context(), inventory.Inventory{Lines: []inventory.Line{changed}}, inventory.Delta{Changed: []inventory.Line{changed}}); err != nil {
		t.Fatal(err)
	}
	oldest, bytes := client.Pending()
	if oldest.IsZero() || bytes != 5 {
		t.Fatalf("pending changed baseline = %v/%d", oldest, bytes)
	}
	reverted := writeVersion(t, path, "base", "normal")
	if err := client.Scan(t.Context(), inventory.Inventory{Lines: []inventory.Line{reverted}}, inventory.Delta{Changed: []inventory.Line{reverted}}); err != nil {
		t.Fatal(err)
	}
	oldest, bytes = client.Pending()
	if !oldest.IsZero() || bytes != 0 {
		t.Fatalf("pending after baseline revert = %v/%d", oldest, bytes)
	}
}

func TestRecoverFindsStoredSnapshotWithoutAck(t *testing.T) {
	localRestic(t)
	repo := filepath.Join(t.TempDir(), "repo")
	client, _, state := openTestClient(t, repo)
	path := filepath.Join(t.TempDir(), "captured.jsonl")
	if err := os.WriteFile(path, []byte("captured"), 0o600); err != nil {
		t.Fatal(err)
	}
	line := inventory.Line{Path: path, Type: "file", Size: 8, SHA256: sha256.Sum256([]byte("captured"))}
	if err := client.Scan(t.Context(), inventory.Inventory{Lines: []inventory.Line{line}}, inventory.Delta{Added: []inventory.Line{line}}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Pass(t.Context(), []string{path}, nil); err != nil {
		t.Fatal(err)
	}
	events, err := os.ReadFile(filepath.Join(state, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(events)), "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.Contains(line, `"event":"pass-acked"`) {
			continue
		}
		kept = append(kept, line)
	}
	if err := os.WriteFile(filepath.Join(state, "journal.jsonl"), []byte(strings.Join(kept, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recovered, err := Open(state, RepoRef{Scope: "k3s-test-one", Repository: repo}, &fakeEscrow{}, WithScratch(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if err := recovered.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	journal, err := os.ReadFile(filepath.Join(state, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(journal), `"event":"pass-stored"`) {
		t.Fatalf("journal lacks stored recovery result: %s", journal)
	}
	oldest, bytes := recovered.Pending()
	if oldest.IsZero() || bytes == 0 {
		t.Fatalf("stored recovery cleared pending: %v/%d", oldest, bytes)
	}
}

// TestOpenDropsTornJournalTail: a crash mid-append leaves a line with no newline; Open must drop it, not fail.
func TestOpenDropsTornJournalTail(t *testing.T) {
	_, _, state := openTestClient(t, filepath.Join(t.TempDir(), "repo"))
	journal := filepath.Join(state, "journal.jsonl")
	before, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	torn := append(append([]byte(nil), before...), []byte(`{"event":"path-acked","passId":"x","pa`)...)
	if err := os.WriteFile(journal, torn, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(state, RepoRef{Scope: "k3s-test-one", Repository: filepath.Join(t.TempDir(), "repo")}, &fakeEscrow{}, WithScratch(t.TempDir())); err != nil {
		t.Fatalf("Open with torn tail: %v", err)
	}
	after, err := os.ReadFile(journal)
	if err != nil || string(after) != string(before) {
		t.Fatalf("torn tail not truncated: %q, %v", after, err)
	}
}

// TestOpenReadOnlySkipsTornJournalTail: read-only Open skips a torn final line and leaves the journal as it was.
func TestOpenReadOnlySkipsTornJournalTail(t *testing.T) {
	_, _, state := openTestClient(t, filepath.Join(t.TempDir(), "repo"))
	journal := filepath.Join(state, "journal.jsonl")
	before, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	torn := append(append([]byte(nil), before...), []byte(`{"event":"path-acked","passId":"x","pa`)...)
	if err := os.WriteFile(journal, torn, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(state, RepoRef{Scope: "k3s-test-one", Repository: filepath.Join(t.TempDir(), "repo")}); err != nil {
		t.Fatalf("OpenReadOnly with torn tail: %v", err)
	}
	after, err := os.ReadFile(journal)
	if err != nil || string(after) != string(torn) {
		t.Fatalf("read-only open changed the journal: %q, %v", after, err)
	}
}

func TestInitBrokenForKeysWithoutConfig(t *testing.T) {
	localRestic(t)
	repo := filepath.Join(t.TempDir(), "repo")
	client, _, _ := openTestClient(t, repo)
	if _, err := client.runJSON(t.Context(), client.commonArgs("init", true)...); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repo, "config")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(client.stateDir, "journal.jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	client.initialized = false
	if err := client.ensureRepository(t.Context()); !errors.Is(err, ErrInitBroken) {
		t.Fatalf("init error = %v, want ErrInitBroken", err)
	}
}

func TestPassContextCancellation(t *testing.T) {
	localRestic(t)
	client, _, _ := openTestClient(t, filepath.Join(t.TempDir(), "repo"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := client.Pass(ctx, []string{"secret-path"}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("pass error = %v, want canceled", err)
	}
}

func TestPendingExcludesFinalOnlyDatabase(t *testing.T) {
	client, _, _ := openTestClient(t, filepath.Join(t.TempDir(), "repo"))
	line := inventory.Line{Path: "agent.db", Class: "final-only", Size: 12}
	if err := client.Scan(t.Context(), inventory.Inventory{Lines: []inventory.Line{line}}, inventory.Delta{Added: []inventory.Line{line}}); err != nil {
		t.Fatal(err)
	}
	oldest, bytes := client.Pending()
	if !oldest.IsZero() || bytes != 0 {
		t.Fatalf("final-only database entered pending: %v/%d", oldest, bytes)
	}
}

func TestSourceErrorsHidePaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret-sentinel", "child")
	if err := os.WriteFile(filepath.Dir(path), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := statLine(path)
	if err == nil || strings.Contains(err.Error(), "secret-sentinel") {
		t.Fatalf("stat error leaks source path: %v", err)
	}
}

func TestResticRelativeArguments(t *testing.T) {
	root := filepath.Join(t.TempDir(), "secret-sentinel")
	c, err := Open(filepath.Join(root, "state"), RepoRef{Scope: "test", Repository: filepath.Join(t.TempDir(), "repo")}, &fakeEscrow{}, WithScratch(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range c.commonArgs("backup", true) {
		if strings.Contains(arg, "secret-sentinel") {
			t.Fatalf("argv leaks state root: %s", arg)
		}
	}
}

func TestScratchOption(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scratch")
	c, _, _ := openTestClient(t, filepath.Join(t.TempDir(), "repo"), WithScratch(dir))
	recorded := filepath.Join(t.TempDir(), "env")
	resticWrapper(t, "printf '%s' \"$TMPDIR\" > "+recorded+"; exit 0")
	if _, err := c.runJSON(t.Context(), "version"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(recorded)
	if err != nil || string(data) != dir {
		t.Fatalf("restic TMPDIR = %q, %v; want %q", data, err, dir)
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("scratch mode: %v, %v", info, err)
	}
}
