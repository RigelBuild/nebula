package client

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReviewInitKeysOnly(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	first, _, _ := openTestClient(t, repo)
	if err := first.ensureRepository(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repo, "config")); err != nil {
		t.Fatal(err)
	}
	second, _, state := openTestClient(t, repo)
	if err := second.ensureRepository(t.Context()); !errors.Is(err, ErrInitBroken) {
		t.Fatalf("init: %v", err)
	}
	if !journalHas(t, state, "init-broken", "") {
		t.Fatal("missing init-broken")
	}
}

func TestReviewInitConfigError(t *testing.T) {
	c, _, state := openTestClient(t, filepath.Join(t.TempDir(), "repo"))
	resticWrapper(t, `case " $* " in *' cat config '*) exit 1 ;; esac; exec REAL_RESTIC "$@"`)
	if err := c.ensureRepository(t.Context()); err == nil || errors.Is(err, ErrInitBroken) {
		t.Fatalf("init: %v", err)
	}
	if journalHas(t, state, "init-broken", "") {
		t.Fatal("unexpected init-broken")
	}
}

func TestReviewRecoverPreservesAck(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	c, _, state := openTestClient(t, repo)
	file := filepath.Join(t.TempDir(), "file")
	line := writeVersion(t, file, "data", "normal")
	scanLines(t, c, line)
	ack := ackFiles(t, c, file)
	scanLines(t, c, line)
	resticWrapper(t, `case " $* " in *' backup '*) kill -KILL "$PPID" ;; *) exec REAL_RESTIC "$@" ;; esac`)
	crashPass(t, state, repo, file)
	c = reopenTestClient(t, state, repo)
	if err := c.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !journalHas(t, state, "pass-abandoned", "") {
		t.Fatal("missing recovery outcome")
	}
	if got := c.latestAcked(); got != ack.SnapshotID {
		t.Fatalf("latest ack = %q, want %q", got, ack.SnapshotID)
	}
	if _, err := c.Pass(t.Context(), nil, nil); !errors.Is(err, ErrPassDone) {
		t.Fatalf("second pass: %v", err)
	}
}
