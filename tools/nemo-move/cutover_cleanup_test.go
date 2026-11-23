package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runCleanupScript runs the cutover cleanup script under /bin/sh with the
// source home and staging path, as productionCutover.cleanup passes them.
func runCleanupScript(t *testing.T, home, staging string) (string, error) {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "/bin/sh", "-c", cleanupStagingScript, "sh", home, staging).CombinedOutput()
	return string(out), err
}

// A symlinked state parent must be refused before anything is removed, and
// the directory it points at must survive intact.
func TestCleanupStagingRefusesSymlinkedParent(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "nemo-move-staging")
	if err := os.MkdirAll(filepath.Join(victim, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".local"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, ".local", "state")); err != nil {
		t.Fatal(err)
	}
	out, err := runCleanupScript(t, home, filepath.Join(home, ".local", "state", "nemo-move-staging"))
	if err == nil || !strings.Contains(out, "refusing symlinked directory") {
		t.Fatalf("cleanup accepted a symlinked state parent: error %v, output %q", err, out)
	}
	if _, err := os.Stat(filepath.Join(victim, "workspace")); err != nil {
		t.Fatalf("cleanup removed the tree behind the symlink: %v", err)
	}
}

// A symlinked staging path is refused, and the target directory it points at
// is left in place rather than removed through the link.
func TestCleanupStagingRefusesSymlinkedStaging(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "credentials")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(home, ".local", "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(stateDir, "nemo-move-staging")
	if err := os.Symlink(target, staging); err != nil {
		t.Fatal(err)
	}
	out, err := runCleanupScript(t, home, staging)
	if err == nil || !strings.Contains(out, "refusing symlinked staging") {
		t.Fatalf("cleanup accepted a symlinked staging path: error %v, output %q", err, out)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("cleanup removed the symlink target: %v", err)
	}
}

// A real staging tree is removed, which is the committed-cutover contract.
func TestCleanupStagingRemovesRealStaging(t *testing.T) {
	// The script names /bin/rm absolutely. Without it the script exits before it
	// removes anything, so this test would prove nothing. Skip rather than fail.
	if _, err := os.Stat("/bin/rm"); err != nil {
		t.Skip("script needs /bin/rm, absent on this host; run on macOS")
	}
	home := t.TempDir()
	staging := filepath.Join(home, ".local", "state", "nemo-move-staging")
	if err := os.MkdirAll(filepath.Join(staging, "workspace", "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := runCleanupScript(t, home, staging)
	if err != nil {
		t.Fatalf("cleanup failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("staging still present after cleanup: %v", err)
	}
}
