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
	return runStageScriptFor(t, home, base, source, "workspace")
}

// runStageScriptFor runs stageCredentialsScript with the given account names as
// positional arguments, as stageShell passes them.
func runStageScriptFor(t *testing.T, home, base, source string, accounts ...string) (string, error) {
	t.Helper()
	args := append([]string{"-c", stageCredentialsScript, "sh", home, base, "example-user:staff", source}, accounts...)
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

// requireStageTools skips when a tool the script names by absolute path is
// missing. Without them the script exits before its account loop, so a test
// that relies on the loop would pass vacuously.
func requireStageTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"/bin/mkdir", "/bin/test", "/usr/bin/mktemp"} {
		if _, err := os.Stat(tool); err != nil {
			t.Skipf("script needs %s, absent on this host; run on macOS", tool)
		}
	}
}

// Account names reach the script as positional arguments, so shell syntax in a
// name is never evaluated. Each name below would create the marker file if the
// loop expanded it through a shell.
func TestStageScriptNeverEvaluatesAccountNames(t *testing.T) {
	requireStageTools(t)
	for name, tmpl := range map[string]string{
		"command substitution": "$(touch MARKER)",
		"backquote":            "`touch MARKER`",
		"semicolon":            "a;touch MARKER",
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			marker := filepath.Join(home, "marker")
			account := strings.ReplaceAll(tmpl, "MARKER", marker)
			base := filepath.Join(home, ".local", "state", "nemo-move-staging")
			out, err := runStageScriptFor(t, home, base, filepath.Join(home, "src"), account)
			if err == nil {
				t.Fatalf("script accepted a missing source for %q:\n%s", account, out)
			}
			if _, statErr := os.Stat(marker); statErr == nil {
				t.Fatalf("shell evaluated account name %q", account)
			}
		})
	}
}

// The staging script names its temporary and previous directories with mktemp.
// A fixed name, such as <base>.old or a $$-suffixed temp, lets a planted
// symlink redirect the root move and removal. This is a text check: the
// symlink-redirect path itself runs only as root on macOS.
func TestStageScriptUsesMktempNames(t *testing.T) {
	for _, want := range []string{
		`temp=$(/usr/bin/mktemp -d "$parent/.nemo-move-staging.XXXXXX")`,
		`old=$(/usr/bin/mktemp -d "$parent/.nemo-move-old.XXXXXX")`,
	} {
		if !strings.Contains(stageCredentialsScript, want) {
			t.Fatalf("staging script lacks %s", want)
		}
	}
	for _, fixed := range []string{"$$", `"$base.old"`} {
		if strings.Contains(stageCredentialsScript, fixed) {
			t.Fatalf("staging script uses predictable path %s", fixed)
		}
	}
}
