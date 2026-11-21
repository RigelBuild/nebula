package transfer

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// denyReadACL makes path unreadable to the current user while its mode bits
// still say 0600. The mode check passes, and only tar's open fails, which is the
// failure G1 reports: a read error that is not a mode-bit refusal.
func denyReadACL(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(t.Context(), "/bin/chmod", "+a", "everyone deny read", path).CombinedOutput(); err != nil {
		t.Fatalf("chmod +a: %v: %s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("/bin/chmod", "-N", path).Run()
	})
}

// seedStaging puts an old, complete staging tree in place. A failed run must
// leave it untouched.
func seedStaging(t *testing.T, stage string) {
	t.Helper()
	dir := filepath.Join(stage, "workspace", "credentials")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token.json"), []byte(`{"refresh":"old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// G1: a root read failure after part of the stream is written must not replace
// the staging tree. The ACL denies read on a 0600 credential, so tar exits 1
// after it has written the other file. The run must fail, and the old staging
// must survive with its old token and no partial new tree.
func TestStagingKeepsOldTreeOnRootReadFailure(t *testing.T) {
	requireDarwin(t)
	home := t.TempDir()
	stage := filepath.Join(home, ".local", "state", "nemo-move-staging")
	seedStaging(t, stage)
	source := fixtureSource(t, `{"refresh":"new"}`, "workspace")
	denied := filepath.Join(source, "workspace", "credentials", "denied.json")
	if err := os.WriteFile(denied, []byte(`{"refresh":"denied"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	denyReadACL(t, denied)
	pipeline := stagePipelineFor(t, home, source, "workspace")
	if err := runPipeline(t, home, pipeline); err == nil {
		t.Fatal("pipeline exited 0 after tar failed on an ACL-denied credential")
	}
	assertOldStagingKept(t, stage)
	assertNoMktempLeftovers(t, filepath.Dir(stage))
}

// G1, printed fallback: the operator command has the same gap and must refuse
// the same way.
func TestPrintedFallbackKeepsOldTreeOnRootReadFailure(t *testing.T) {
	requireDarwin(t)
	home := t.TempDir()
	stage := filepath.Join(home, ".local", "state", "nemo-move-staging")
	seedStaging(t, stage)
	source := fixtureSource(t, `{"refresh":"new"}`, "workspace")
	denied := filepath.Join(source, "workspace", "credentials", "denied.json")
	if err := os.WriteFile(denied, []byte(`{"refresh":"denied"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	denyReadACL(t, denied)
	opts := Options{SourceHome: home, MacUser: "example-user", Layout: testLayout}
	opts.Layout.Google.Accounts = []string{"workspace"}
	opts.Layout.Google.Source = source
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", printedFallback(t, opts))
	cmd.Env = append(os.Environ(), "HOME="+home)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("printed fallback exited 0 after tar failed on an ACL-denied credential:\n%s", out)
	}
	assertOldStagingKept(t, stage)
	assertNoMktempLeftovers(t, filepath.Dir(stage))
}

// assertOldStagingKept fails unless the seeded old token is still staged and no
// new, partial token replaced it.
func assertOldStagingKept(t *testing.T, stage string) {
	t.Helper()
	token := filepath.Join(stage, "workspace", "credentials", "token.json")
	got, err := os.ReadFile(token)
	if err != nil {
		t.Fatalf("old staging was removed by a failed run: %v", err)
	}
	if string(got) != `{"refresh":"old"}` {
		t.Fatalf("failed run replaced the old staging; token = %q", got)
	}
	if _, err := os.Stat(filepath.Join(stage, "workspace", "credentials", "denied.json")); !os.IsNotExist(err) {
		t.Fatalf("failed run staged a partial tree: denied.json present (%v)", err)
	}
}
