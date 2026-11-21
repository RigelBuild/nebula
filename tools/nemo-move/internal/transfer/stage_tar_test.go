package transfer

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// requireDarwin skips the staging tests off macOS. They drive the staging
// scripts with /bin and /usr/bin tools and assert macOS staff ownership.
func requireDarwin(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("staging tests run on macOS; run them on macmini")
	}
}

// currentOwner returns the user:group of the test process. The staged tree must
// end up owned by the SSH user, never by root.
func currentOwner(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	g, err := user.LookupGroupId(u.Gid)
	if err != nil {
		t.Fatal(err)
	}
	return u.Username + ":" + g.Name
}

// fixtureSource writes <source>/<account>/credentials/token.json for each
// account, owned by the test user, as the Mac Google source looks before staging.
func fixtureSource(t *testing.T, token string, accounts ...string) string {
	t.Helper()
	source := filepath.Join(t.TempDir(), "accounts")
	for _, account := range accounts {
		dir := filepath.Join(source, account, "credentials")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "token.json"), []byte(token), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return source
}

// rootHalf is the root side of staging. It reads the Google source and returns
// the archive it streams.
//
// STUB: this runs the current root script, which writes the staging tree
// itself and streams no archive. The M1 change replaces this body.
func rootHalf(t *testing.T, home, stage, source string, accounts ...string) ([]byte, error) {
	t.Helper()
	args := append([]string{"-c", stageCredentialsScript, "sh", home, stage, currentOwner(t), source}, accounts...)
	out, err := exec.CommandContext(t.Context(), "/bin/sh", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, out)
	}
	return nil, nil
}

// unpackStagingAsUser is the user side of staging. It unpacks the root's archive
// into a mktemp dir the user owns, then renames it into place.
//
// STUB: this keeps the current behaviour, where the root script already wrote
// the staging tree and there is no separate user step.
func unpackStagingAsUser(t *testing.T, home, stage string, archive []byte) error {
	t.Helper()
	return nil
}

// snapshotTree records every path under root with its owner, group, mode and
// mtime, so a test can detect any write or chown.
func snapshotTree(t *testing.T, root string) []string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatalf("no stat for %s", path)
		}
		lines = append(lines, fmt.Sprintf("%s uid=%d gid=%d mode=%s mtime=%d", path, st.Uid, st.Gid, info.Mode(), info.ModTime().UnixNano()))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return lines
}

// changedLines lists the snapshot lines that differ between before and after.
func changedLines(before, after []string) []string {
	beforeSet := make(map[string]bool, len(before))
	for _, line := range before {
		beforeSet[line] = true
	}
	afterSet := make(map[string]bool, len(after))
	for _, line := range after {
		afterSet[line] = true
	}
	var changed []string
	for _, line := range after {
		if !beforeSet[line] {
			changed = append(changed, "+ "+line)
		}
	}
	for _, line := range before {
		if !afterSet[line] {
			changed = append(changed, "- "+line)
		}
	}
	return changed
}

// assertStagedAs checks the staged tree a successful run leaves: the new token,
// owned by uid, 700 directories and 600 files, and no mktemp leftovers beside it.
func assertStagedAs(t *testing.T, stage string, uid int) {
	t.Helper()
	token := filepath.Join(stage, "workspace", "credentials", "token.json")
	got, err := os.ReadFile(token)
	if err != nil {
		t.Fatalf("staged token missing: %v", err)
	}
	if string(got) != `{"refresh":"new"}` {
		t.Fatalf("staged token = %q, want the new token", got)
	}
	assertMode(t, token, uid, 0o600)
	assertMode(t, filepath.Dir(token), uid, 0o700)
	entries, err := os.ReadDir(filepath.Dir(stage))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".nemo-move-") {
			t.Fatalf("staging left %s behind", entry.Name())
		}
	}
}

// assertMode checks one path's permission bits and owner uid.
func assertMode(t *testing.T, path string, uid int, perm os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != perm {
		t.Fatalf("%s mode = %o, want %o", path, info.Mode().Perm(), perm)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("no stat for %s", path)
	}
	if int(st.Uid) != uid {
		t.Fatalf("%s owner uid = %d, want %d", path, st.Uid, uid)
	}
}

// M1: root must never write in the user's tree. The root side reads the Google
// source and streams it out, so it must leave the user's home exactly as found.
func TestStagingRootNeverWritesUserTree(t *testing.T) {
	requireDarwin(t)
	home := t.TempDir()
	stage := filepath.Join(home, ".local", "state", "nemo-move-staging")
	if err := os.MkdirAll(filepath.Join(stage, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "sentinel"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := fixtureSource(t, `{"refresh":"new"}`, "workspace")
	before := snapshotTree(t, home)
	archive, err := rootHalf(t, home, stage, source, "workspace")
	if err != nil {
		t.Fatalf("root half failed: %v", err)
	}
	if changed := changedLines(before, snapshotTree(t, home)); len(changed) != 0 {
		t.Fatalf("root half changed the user tree:\n%s", strings.Join(changed, "\n"))
	}
	if len(archive) == 0 {
		t.Fatal("root half streamed no archive; root must stream the accounts as tar")
	}
}

// M1: the user side unpacks the archive into a mktemp dir it owns, then renames
// it into place. The staged tree must be owned by the SSH user, not root.
func TestStagingUserUnpackIsUserOwned(t *testing.T) {
	requireDarwin(t)
	home := t.TempDir()
	stage := filepath.Join(home, ".local", "state", "nemo-move-staging")
	if err := os.MkdirAll(filepath.Dir(stage), 0o700); err != nil {
		t.Fatal(err)
	}
	source := fixtureSource(t, `{"refresh":"new"}`, "workspace")
	archive, err := rootHalf(t, home, stage, source, "workspace")
	if err != nil {
		t.Fatalf("root half failed: %v", err)
	}
	if len(archive) == 0 {
		t.Fatal("root half streamed no archive; root must stream the accounts as tar")
	}
	if err := unpackStagingAsUser(t, home, stage, archive); err != nil {
		t.Fatalf("user unpack failed: %v", err)
	}
	assertStagedAs(t, stage, os.Getuid())
}

// M1: a symlinked user parent is refused by the user side before anything is
// unpacked, and nothing lands outside the home. The root side reads only the
// Google source, so it must not refuse because of the user's tree.
func TestStagingUserUnpackRefusesSymlinkedParent(t *testing.T) {
	requireDarwin(t)
	home := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".local"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, ".local", "state")); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(home, ".local", "state", "nemo-move-staging")
	source := fixtureSource(t, `{"refresh":"new"}`, "workspace")
	archive, err := rootHalf(t, home, stage, source, "workspace")
	if err != nil {
		t.Fatalf("root half refused the user's tree; it must read only the Google source: %v", err)
	}
	if err := unpackStagingAsUser(t, home, stage, archive); err == nil {
		t.Fatal("user unpack accepted a symlinked state parent")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("user unpack wrote %d entries outside the home", len(entries))
	}
}

// M2: the staging success path. A run replaces an old staged tree with the new
// token, leaves no old tree and no .nemo-move-* leftovers, and sets 700 dirs and
// 600 files owned by the SSH user. This test guards the success path through the
// M1 rewrite.
func TestStagingSuccessReplacesOldTree(t *testing.T) {
	requireDarwin(t)
	home := t.TempDir()
	stage := filepath.Join(home, ".local", "state", "nemo-move-staging")
	oldDir := filepath.Join(stage, "workspace", "credentials")
	if err := os.MkdirAll(oldDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldDir, "token.json"), []byte(`{"refresh":"old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "old-sentinel"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := fixtureSource(t, `{"refresh":"new"}`, "workspace")
	archive, err := rootHalf(t, home, stage, source, "workspace")
	if err != nil {
		t.Fatalf("root half failed: %v", err)
	}
	if err := unpackStagingAsUser(t, home, stage, archive); err != nil {
		t.Fatalf("user unpack failed: %v", err)
	}
	assertStagedAs(t, stage, os.Getuid())
	if _, err := os.Stat(filepath.Join(stage, "old-sentinel")); !os.IsNotExist(err) {
		t.Fatalf("old tree survived the run: %v", err)
	}
}
