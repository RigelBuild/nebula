//go:build linux

package transfer

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func useTestSealer() func() {
	old := journalSystemOps
	journalSystemOps = journalOps{
		get: func(uintptr) (uint32, error) { return appendFlag, nil },
		set: func(uintptr, uint32) error { return nil },
		own: func(int, int, int) error { return nil },
	}
	return func() { journalSystemOps = old }
}

func TestSealReturnsPlatformResult(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Seal(dir); err != nil {
		if errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EPERM) {
			t.Skipf("filesystem cannot seal journal: %v", err)
		}
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	flags, err := getFileFlags(file.Fd())
	if err != nil {
		file.Close()
		t.Fatal(err)
	}
	if flags&appendFlag == 0 {
		file.Close()
		t.Fatal("append-only flag was not set")
	}
	if err := setFileFlags(file.Fd(), flags&^appendFlag); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReplaceJournalsInitializedMissing(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, pageRoot)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".initialized"), nil, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pages.jsonl.incoming"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := Manifest{Entries: []ManifestEntry{{Path: pageRoot + "/pages.jsonl", Type: fileType}}}
	if err := ReplaceJournals(root, m); err == nil {
		t.Fatal("missing initialized journal accepted")
	}
	if _, err := os.Lstat(filepath.Join(dir, "pages.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("journal created: %v", err)
	}
}

func TestReplaceJournalsNoDefaultsAfterInitialization(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "existing"}[existing], func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, pageRoot)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".initialized"), nil, 0o444); err != nil {
				t.Fatal(err)
			}
			if existing {
				if err := os.WriteFile(filepath.Join(dir, "pages.jsonl"), []byte("preserved"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(useTestSealer())
			if err := ReplaceJournals(root, Manifest{}); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(dir, "pages.jsonl"))
			if existing && (err != nil || string(data) != "preserved") {
				t.Fatalf("journal = %q, %v", data, err)
			}
			if !existing && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("journal created: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "linear-issues.jsonl")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("default journal created: %v", err)
			}
		})
	}
}

func TestReplaceJournalsRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, pageRoot)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("outside", filepath.Join(dir, "pages.jsonl")); err != nil {
		t.Fatal(err)
	}
	m := Manifest{Entries: []ManifestEntry{{Path: pageRoot + "/pages.jsonl", Type: fileType}}}
	if err := ReplaceJournals(root, m); err == nil {
		t.Fatal("symlink accepted")
	}
	if _, err := os.Lstat(filepath.Join(dir, ".initialized")); !os.IsNotExist(err) {
		t.Fatalf("initialization claimed: %v", err)
	}
}

func TestReplaceJournalOrderAndFailures(t *testing.T) {
	for _, fail := range []string{"", "get-old", "own", "unseal", "get-new", "seal", "verify"} {
		t.Run(fail, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "pages.jsonl"), []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "pages.jsonl.incoming"), []byte("new"), 0o600); err != nil {
				t.Fatal(err)
			}
			fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(fd)
			var calls []string
			var newFlag uint32
			gets := 0
			ops := journalOps{
				get: func(uintptr) (uint32, error) {
					gets++
					step := "verify"
					if gets == 1 {
						step = "get-old"
					} else if gets == 2 {
						step = "get-new"
					}
					calls = append(calls, step)
					if fail == step {
						return 0, errors.New(step)
					}
					if gets == 1 {
						return appendFlag, nil
					}
					return newFlag, nil
				},
				set: func(_ uintptr, flag uint32) error {
					step := "seal"
					if flag&appendFlag == 0 {
						step = "unseal"
					}
					calls = append(calls, step)
					if fail == step {
						return errors.New(step)
					}
					newFlag = flag
					return nil
				},
				own: func(int, int, int) error {
					calls = append(calls, "own")
					if fail == "own" {
						return errors.New("own")
					}
					return nil
				},
			}
			err = replaceJournal(fd, "pages.jsonl", true, ops)
			if fail != "" {
				if err == nil {
					t.Fatalf("%s failure accepted", fail)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(calls, ","); got != "get-old,own,unseal,get-new,seal,verify" {
				t.Fatalf("order: %s", got)
			}
			content, err := os.ReadFile(filepath.Join(dir, "pages.jsonl"))
			if err != nil || string(content) != "new" {
				t.Fatalf("content %q: %v", content, err)
			}
			if newFlag&appendFlag == 0 {
				t.Fatal("not sealed")
			}
		})
	}
}

func TestReplaceJournalFirstInit(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pages.jsonl.incoming"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	var flags uint32
	ops := journalOps{get: func(uintptr) (uint32, error) { return flags, nil }, set: func(_ uintptr, f uint32) error { flags = f; return nil }, own: func(int, int, int) error { return nil }}
	if err := replaceJournal(fd, "pages.jsonl", false, ops); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(dir, "pages.jsonl"))
	if err != nil || string(content) != "new" {
		t.Fatalf("content %q: %v", content, err)
	}
	if flags&appendFlag == 0 {
		t.Fatal("not sealed")
	}
}

func TestReplaceJournalKernel(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root and CAP_LINUX_IMMUTABLE")
	}
	root := t.TempDir()
	dir := filepath.Join(root, pageRoot)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "pages.jsonl")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Seal(dir); err != nil {
		if errors.Is(err, unix.EPERM) || errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.EOPNOTSUPP) {
			t.Skipf("append-only unavailable: %v", err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f, err := os.Open(path)
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		flags, err := getFileFlags(f.Fd())
		if err == nil {
			err = setFileFlags(f.Fd(), flags&^appendFlag)
		}
		if err != nil {
			t.Error(err)
		}
	})
	if err := os.WriteFile(filepath.Join(dir, ".initialized"), nil, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".incoming", []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := Manifest{Entries: []ManifestEntry{{Path: pageRoot + "/pages.jsonl", Type: fileType}}}
	if err := ReplaceJournals(root, m); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	flags, err := getFileFlags(f.Fd())
	if err != nil || flags&appendFlag == 0 {
		t.Fatalf("flags %x: %v", flags, err)
	}
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if stat.Uid != 10000 || stat.Gid != 10000 || info.Mode().Perm() != 0o600 {
		t.Fatalf("owner/mode: %d:%d %o", stat.Uid, stat.Gid, info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "new" {
		t.Fatalf("content %q: %v", data, err)
	}
}
