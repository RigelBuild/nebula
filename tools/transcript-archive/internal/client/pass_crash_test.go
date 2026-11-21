package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func resticWrapper(t *testing.T, body string) {
	t.Helper()
	real, err := exec.LookPath("restic")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n" + strings.ReplaceAll(body, "REAL_RESTIC", real) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "restic"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func journalHas(t *testing.T, state, event, path string) bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(state, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		var e journalEvent
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		if e.Event == event && (path == "" || e.Path == path) {
			return true
		}
	}
	return false
}

func reopenTestClient(t *testing.T, state, repo string) *Client {
	t.Helper()
	c, err := Open(state, RepoRef{Scope: "k3s-test-one", Repository: repo}, &fakeEscrow{}, WithScratch(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCrashPassHelper(t *testing.T) {
	if os.Getenv("CLIENT_CRASH_HELPER") != "1" {
		return
	}
	c := reopenTestClient(t, os.Getenv("CLIENT_STATE"), os.Getenv("CLIENT_REPO"))
	_, err := c.Pass(t.Context(), []string{os.Getenv("CLIENT_FILE")}, nil)
	t.Fatalf("pass unexpectedly returned: %v", err)
}

func crashPass(t *testing.T, state, repo, file string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashPassHelper$")
	cmd.Env = append(os.Environ(), "CLIENT_CRASH_HELPER=1", "CLIENT_STATE="+state, "CLIENT_REPO="+repo, "CLIENT_FILE="+file)
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != -1 {
		t.Fatalf("child exit = %v, want signal", err)
	}
}

func TestRecoverCrashPoints(t *testing.T) {
	for _, tc := range []struct{ name, script, event string }{
		{name: "stored", script: `REAL_RESTIC "$@" || exit 1; kill -KILL "$PPID"`, event: "pass-stored"},
		{name: "not uploaded", script: `kill -KILL "$PPID"`, event: "pass-abandoned"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			localRestic(t)
			repo := filepath.Join(t.TempDir(), "repo")
			c, _, state := openTestClient(t, repo)
			file := filepath.Join(t.TempDir(), "session")
			line := writeVersion(t, file, "data", "normal")
			scanLines(t, c, line)
			if err := c.ensureRepository(t.Context()); err != nil {
				t.Fatal(err)
			}
			resticWrapper(t, "case \" $* \" in *' backup '*) "+tc.script+" ;; *) exec REAL_RESTIC \"$@\" ;; esac")
			crashPass(t, state, repo, file)
			c = reopenTestClient(t, state, repo)
			if err := c.Recover(t.Context()); err != nil {
				t.Fatal(err)
			}
			if !journalHas(t, state, tc.event, "") {
				t.Fatalf("missing %s", tc.event)
			}
			if oldest, size := c.Pending(); oldest.IsZero() || size != 4 {
				t.Fatalf("pending = %v/%d", oldest, size)
			}
			if tc.event == "pass-abandoned" {
				// Restore the real binary for the next pass.
				t.Setenv("PATH", strings.TrimPrefix(os.Getenv("PATH"), strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]+string(os.PathListSeparator)))
				scanLines(t, c, line)
				ackFiles(t, c, file)
			}
		})
	}
}

func TestInitWrongPassword(t *testing.T) {
	localRestic(t)
	repo := filepath.Join(t.TempDir(), "repo")
	first, _, _ := openTestClient(t, repo)
	if err := first.ensureRepository(t.Context()); err != nil {
		t.Fatal(err)
	}
	second, _, state := openTestClient(t, repo)
	if err := second.ensureRepository(t.Context()); !errors.Is(err, ErrInitBroken) {
		t.Fatalf("init = %v", err)
	}
	if !journalHas(t, state, "init-broken", "") {
		t.Fatal("missing init-broken")
	}
}

func TestInitRetriesEmptyRepository(t *testing.T) {
	localRestic(t)
	repo := filepath.Join(t.TempDir(), "repo")
	c, _, _ := openTestClient(t, repo)
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "failed")
	resticWrapper(t, fmt.Sprintf(`case " $* " in *' init '*) if [ ! -e %q ]; then : > %q; exit 1; fi ;; esac
exec REAL_RESTIC "$@"`, marker, marker))
	if err := c.ensureRepository(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, "config")); err != nil {
		t.Fatal(err)
	}
}
func TestVanishedDuringPass(t *testing.T) {
	localRestic(t)
	repo := filepath.Join(t.TempDir(), "repo")
	c, _, state := openTestClient(t, repo)
	dir := t.TempDir()
	gone := filepath.Join(dir, "gone")
	unreadable := filepath.Join(dir, "unreadable")
	first := writeVersion(t, gone, "gone", "normal")
	second := writeVersion(t, unreadable, "unreadable", "normal")
	scanLines(t, c, first, second)
	if err := c.ensureRepository(t.Context()); err != nil {
		t.Fatal(err)
	}
	resticWrapper(t, fmt.Sprintf(`case " $* " in *' backup '*) rm -- %q; chmod 000 -- %q ;; esac
exec REAL_RESTIC "$@"`, gone, unreadable))
	outcome, err := c.Pass(t.Context(), []string{gone, unreadable}, nil)
	if err != nil || outcome.Kind != Incomplete || outcome.ExitCode != 3 || outcome.SnapshotID == "" {
		t.Fatalf("pass = %#v, %v", outcome, err)
	}
	if !journalHas(t, state, "vanished", gone) || journalHas(t, state, "vanished", unreadable) {
		t.Fatal("incorrect vanished journal entries")
	}
	if _, ok := c.pending[pathKey(unreadable)]; !ok {
		t.Fatal("unreadable file not pending")
	}
	if _, ok := c.pending[pathKey(gone)]; ok {
		t.Fatal("vanished file still pending")
	}
}

func TestIncompleteSurvivesCrashUntilAck(t *testing.T) {
	localRestic(t)
	repo := filepath.Join(t.TempDir(), "repo")
	c, _, state := openTestClient(t, repo)
	file := filepath.Join(t.TempDir(), "session")
	line := writeVersion(t, file, "data", "normal")
	scanLines(t, c, line)
	if err := c.ensureRepository(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0); err != nil {
		t.Fatal(err)
	}
	outcome, err := c.Pass(t.Context(), []string{file}, nil)
	if err != nil || outcome.Kind != Incomplete || outcome.ExitCode != 3 || outcome.SnapshotID == "" {
		t.Fatalf("pass = %#v, %v", outcome, err)
	}
	scanLines(t, c, line)
	resticWrapper(t, `case " $* " in *' backup '*) kill -KILL "$PPID" ;; *) exec REAL_RESTIC "$@" ;; esac`)
	crashPass(t, state, repo, file)
	c = reopenTestClient(t, state, repo)
	if err := c.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if oldest, size := c.Pending(); oldest.IsZero() || size != 4 {
		t.Fatalf("pending = %v/%d", oldest, size)
	}
	if err := os.Chmod(file, 0o600); err != nil {
		t.Fatal(err)
	}
	_, path, _ := strings.Cut(os.Getenv("PATH"), string(os.PathListSeparator))
	t.Setenv("PATH", path)
	scanLines(t, c, line)
	ackFiles(t, c, file)
	if oldest, size := c.Pending(); !oldest.IsZero() || size != 0 {
		t.Fatalf("pending = %v/%d", oldest, size)
	}
}
