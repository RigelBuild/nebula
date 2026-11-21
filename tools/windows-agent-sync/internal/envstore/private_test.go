package envstore

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// fixedThenUnique returns all-zero randomness first, then all-ones.
func fixedThenUnique() func([]byte) (int, error) {
	calls := 0
	return func(buf []byte) (int, error) {
		fill := byte(0)
		if calls > 0 {
			fill = 0xff
		}
		calls++
		for index := range buf {
			buf[index] = fill
		}
		return len(buf), nil
	}
}

func TestCreatePrivateFileSkipsExistingName(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, ".example-0000000000000000.tmp")
	if err := os.WriteFile(existing, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	original := readRandom
	readRandom = fixedThenUnique()
	t.Cleanup(func() { readRandom = original })

	file, err := CreatePrivateFile(dir, ".example-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, ".example-ffffffffffffffff.tmp"); file.Name() != want {
		t.Fatalf("created %s, want %s", file.Name(), want)
	}
	data, err := os.ReadFile(existing) //nolint:gosec // existing is under t.TempDir.
	if err != nil || string(data) != "keep" {
		t.Fatalf("existing file = %q, %v; want it untouched", data, err)
	}
}

func TestMkdirPrivateTightensExistingDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix mode bits differ on Windows; TestPrivateDACL covers it")
	}
	dir := filepath.Join(t.TempDir(), "secrets")
	if err := os.Mkdir(dir, 0o755); err != nil { //nolint:gosec // the test starts from a loose mode on purpose.
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // undo umask so the loose mode is real.
		t.Fatal(err)
	}
	if err := MkdirPrivate(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Fatalf("dir mode = %o, want 700", mode)
	}
}
