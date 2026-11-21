package transfer

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runStageScript runs stageCredentialsScript under /bin/sh for one account
// named workspace. The arguments are the home, the staging path, the owner, and
// the Google source, as stageShell passes them.
func runStageScript(t *testing.T, home, base, source string) (string, error) {
	t.Helper()
	args := []string{"-c", stageCredentialsScript, "sh", home, base, "example-user:staff", source, "workspace"}
	out, err := exec.CommandContext(t.Context(), "/bin/sh", args...).CombinedOutput()
	return string(out), err
}

// A planted symlink in the staging parent must be refused before any copy or
// move. The script must not create or remove anything outside the home.
func TestStageScriptRefusesSymlinkedParent(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".local"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, ".local", "state")); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(home, ".local", "state", "nemo-move-staging")
	out, err := runStageScript(t, home, base, filepath.Join(home, "src"))
	if err == nil {
		t.Fatalf("script accepted a symlinked state parent:\n%s", out)
	}
	if !strings.Contains(out, "refusing symlinked directory") {
		t.Fatalf("refusal lacks the symlink message:\n%s", out)
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("script wrote %d entries outside the home through the symlink", len(entries))
	}
}

// A failed run removes only the mktemp directories it made. A predictable
// <base>.old, an existing staging tree, and their contents all survive.
func TestStageScriptKeepsPathsItDidNotCreate(t *testing.T) {
	// The script names /bin and /usr/bin tools absolutely. Without them the
	// script exits before it reaches the copy or move, so this test would prove
	// nothing about removal. Skip rather than pass vacuously.
	for _, tool := range []string{"/bin/mkdir", "/usr/bin/mktemp"} {
		if _, err := os.Stat(tool); err != nil {
			t.Skipf("script needs %s, absent on this host; run on macOS", tool)
		}
	}
	home := t.TempDir()
	stateDir := filepath.Join(home, ".local", "state")
	base := filepath.Join(stateDir, "nemo-move-staging")
	token := filepath.Join(base, "workspace", "credentials", "token.json")
	sentinel := base + ".old/sentinel"
	for _, file := range []string{token, sentinel} {
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The source has no credentials, so the script fails at its first account
	// check, before it copies or moves anything.
	out, err := runStageScript(t, home, base, filepath.Join(home, "missing"))
	if err == nil {
		t.Fatalf("script succeeded without credentials:\n%s", out)
	}
	for _, kept := range []string{token, sentinel} {
		if _, err := os.Stat(kept); err != nil {
			t.Fatalf("script removed %s: %v\n%s", kept, err, out)
		}
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".nemo-move-staging.") || strings.HasPrefix(entry.Name(), ".nemo-move-old.") {
			t.Fatalf("failed run left its mktemp directory %s behind", entry.Name())
		}
	}
}
