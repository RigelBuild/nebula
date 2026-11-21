package seat

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// realFiles is the production filesystem, so these tests exercise real modes, symlinks, hardlinks and ownership.
func realFiles() Files {
	return Files{Mkdir: os.MkdirAll, OpenSeatDir: openSeatDirFiles}
}

// modeOf returns the permission bits of path, following no symlink.
func modeOf(t *testing.T, path string) fs.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

func TestSeatDirectoryLooseModeIsTightenedToPrivate(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "seats")
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	deps := Deps{Files: realFiles()}
	if _, err := writeSeatConfig(deps, dir, "seat"); err != nil {
		t.Fatalf("writeSeatConfig: %v", err)
	}
	assertDeepEqual(t, modeOf(t, dir), fs.FileMode(0o700))
}

func TestSeatDirectorySymlinkIsRefused(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "elsewhere")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "seats")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	deps := Deps{Files: realFiles()}
	if _, err := writeSeatConfig(deps, link, "seat"); err == nil {
		t.Fatal("writeSeatConfig through a symlinked directory returned nil error")
	}
	if _, err := os.Lstat(filepath.Join(target, "seat.conf")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("seat.conf was written through the symlinked directory: %v", err)
	}
}

func TestSeatDirectoryOwnedByAnotherUIDIsRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "seats")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	restore := currentUID
	currentUID = func() int { return os.Getuid() + 1 }
	t.Cleanup(func() { currentUID = restore })
	deps := Deps{Files: realFiles()}
	if _, err := writeSeatConfig(deps, dir, "seat"); err == nil {
		t.Fatal("writeSeatConfig in a directory owned by another uid returned nil error")
	}
	if _, err := os.Lstat(filepath.Join(dir, "seat.conf")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("seat.conf was written into a directory owned by another uid: %v", err)
	}
}

func TestSeatConfigSymlinkPlantedAtTheConfigPathIsReplacedAndTargetUntouched(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "seats")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(root, "victim")
	const original = "do not touch\n"
	if err := os.WriteFile(victim, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, "seat.conf")); err != nil {
		t.Fatal(err)
	}
	deps := Deps{Files: realFiles()}
	if _, err := writeSeatConfig(deps, dir, "seat"); err != nil {
		t.Fatalf("writeSeatConfig: %v", err)
	}
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	assertDeepEqual(t, string(got), original)
	assertDeepEqual(t, modeOf(t, victim), fs.FileMode(0o644))
	assertDeepEqual(t, modeOf(t, filepath.Join(dir, "seat.conf")), fs.FileMode(0o600))
}

func TestSeatConfigHardlinkPlantedAtTheConfigPathIsReplacedAndTargetUntouched(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "seats")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(root, "victim")
	const original = "do not touch\n"
	if err := os.WriteFile(victim, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(victim, filepath.Join(dir, "seat.conf")); err != nil {
		t.Fatal(err)
	}
	deps := Deps{Files: realFiles()}
	if _, err := writeSeatConfig(deps, dir, "seat"); err != nil {
		t.Fatalf("writeSeatConfig: %v", err)
	}
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	assertDeepEqual(t, string(got), original)
	assertDeepEqual(t, modeOf(t, victim), fs.FileMode(0o644))
	assertDeepEqual(t, modeOf(t, filepath.Join(dir, "seat.conf")), fs.FileMode(0o600))
}

func TestWriteSeatFileTruncatesAnExistingLooseFileToPrivateMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seat.conf")
	if err := os.WriteFile(path, []byte("an old, longer, looser config\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	seatDir, err := openSeatDirectory(dir)
	if err != nil {
		t.Fatalf("openSeatDirectory: %v", err)
	}
	defer seatDir.Close()
	if err := seatDir.WriteFile("seat.conf", seatConfig); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	assertDeepEqual(t, got, seatConfig)
	assertDeepEqual(t, modeOf(t, path), fs.FileMode(0o600))
}
