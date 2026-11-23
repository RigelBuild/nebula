package transfer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runRootStream runs the root half, the tar streamer, for the given accounts and
// returns its stdout. The test runs it without sudo.
func runRootStream(t *testing.T, source string, accounts ...string) ([]byte, error) {
	t.Helper()
	return rootHalf(t, source, accounts...)
}

// runUnpackStream runs the user half on the archive for the given accounts and
// returns the error, which carries the script's stderr.
func runUnpackStream(t *testing.T, home, stage string, archive []byte, accounts ...string) error {
	t.Helper()
	return unpackStagingAsUser(t, home, stage, archive, accounts...)
}

// The root half reads the Google source and writes only the archive. It must
// fail, and write nothing, when an account has no credentials.
func TestRootStreamFailsOnMissingCredentials(t *testing.T) {
	requireStageTools(t)
	source := t.TempDir()
	archive, err := runRootStream(t, source, "workspace")
	if err == nil {
		t.Fatalf("root stream succeeded without credentials; archive %d bytes", len(archive))
	}
	if len(archive) != 0 {
		t.Fatalf("root stream wrote %d bytes before failing", len(archive))
	}
}

// A planted symlink in the user's staging parent must be refused by the user
// half before any unpack, and nothing may land outside the home.
func TestUnpackRefusesSymlinkedParent(t *testing.T) {
	requireStageTools(t)
	home := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".local"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, ".local", "state")); err != nil {
		t.Fatal(err)
	}
	source := fixtureSource(t, `{"refresh":"new"}`, "workspace")
	archive, err := runRootStream(t, source, "workspace")
	if err != nil {
		t.Fatalf("root stream failed: %v", err)
	}
	stage := filepath.Join(home, ".local", "state", "nemo-move-staging")
	err = runUnpackStream(t, home, stage, archive, "workspace")
	if err == nil {
		t.Fatal("unpack accepted a symlinked state parent")
	}
	if !strings.Contains(err.Error(), "refusing symlinked directory") {
		t.Fatalf("refusal lacks the symlink message: %v", err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("unpack wrote %d entries outside the home through the symlink", len(entries))
	}
}

// A failed unpack removes only the mktemp directories it made. A predictable
// <base>.old, an existing staging tree, and their contents all survive.
func TestUnpackKeepsPathsItDidNotCreate(t *testing.T) {
	requireStageTools(t)
	home := t.TempDir()
	stateDir := filepath.Join(home, ".local", "state")
	stage := filepath.Join(stateDir, "nemo-move-staging")
	token := filepath.Join(stage, "workspace", "credentials", "token.json")
	sentinel := stage + ".old/sentinel"
	for _, file := range []string{token, sentinel} {
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// An empty archive makes the user half fail, before it moves anything into
	// place.
	if err := runUnpackStream(t, home, stage, []byte{}, "workspace"); err == nil {
		t.Fatal("unpack succeeded on an empty archive")
	}
	for _, kept := range []string{token, sentinel} {
		if _, err := os.Stat(kept); err != nil {
			t.Fatalf("unpack removed %s: %v", kept, err)
		}
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".nemo-move-staging.") || strings.HasPrefix(entry.Name(), ".nemo-move-old.") {
			t.Fatalf("failed unpack left its mktemp directory %s behind", entry.Name())
		}
	}
}

// requireStageTools skips when a tool the scripts name by absolute path is
// missing. Without them a script exits before its account loop, so a test that
// relies on that loop would pass vacuously.
func requireStageTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"/bin/mkdir", "/bin/test", "/bin/chmod", "/usr/bin/mktemp", "/usr/bin/tar"} {
		if _, err := os.Stat(tool); err != nil {
			t.Skipf("script needs %s, absent on this host; run on macOS", tool)
		}
	}
}

// Account names reach the root and user scripts as positional arguments, so
// shell syntax in a name is never evaluated. Each name below would create the
// marker file if an argument were expanded by the shell.
func TestStageScriptsNeverEvaluateAccountNames(t *testing.T) {
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
			source := filepath.Join(home, "src")
			if err := os.MkdirAll(source, 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err := runRootStream(t, source, account); err == nil {
				t.Fatalf("root stream accepted a missing source for %q", account)
			}
			stage := filepath.Join(home, ".local", "state", "nemo-move-staging")
			if err := runUnpackStream(t, home, stage, []byte{}, account); err == nil {
				t.Fatalf("unpack accepted an empty archive for %q", account)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatalf("shell evaluated account name %q", account)
			}
		})
	}
}

// The user half names its temporary and previous directories with mktemp. A
// fixed name, such as <base>.old or a $$-suffixed temp, lets a planted symlink
// redirect a move. This is a text check of the script.
func TestUnpackScriptUsesMktempNames(t *testing.T) {
	for _, want := range []string{
		`temp=$(/usr/bin/mktemp -d "$parent/.nemo-move-staging.XXXXXX")`,
		`old=$(/usr/bin/mktemp -d "$parent/.nemo-move-old.XXXXXX")`,
	} {
		if !strings.Contains(stageUnpackScript, want) {
			t.Fatalf("unpack script lacks %s", want)
		}
	}
	for _, fixed := range []string{"$$", `"$base.old"`} {
		if strings.Contains(stageUnpackScript, fixed) {
			t.Fatalf("unpack script uses predictable path %s", fixed)
		}
	}
}

// The root script must not write, chown, move or remove anything. It may only
// read the source and stream the archive. This is a text check of the script.
func TestRootScriptWritesNothing(t *testing.T) {
	for _, forbidden := range []string{"chown", "/bin/mv", "/bin/rm", "/bin/mkdir", "/bin/cp", "chmod", "> ", "touch"} {
		if strings.Contains(stageCredentialsScript, forbidden) {
			t.Fatalf("root script contains write operation %q", forbidden)
		}
	}
	if !strings.Contains(stageCredentialsScript, "/usr/bin/tar -c") {
		t.Fatal("root script does not stream the accounts as tar")
	}
}
