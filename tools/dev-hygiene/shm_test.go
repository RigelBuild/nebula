package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPruneShm(t *testing.T) {
	now := time.Now()
	for _, tt := range []struct {
		name                                             string
		apply, recent, owned, heldRoot, heldFile, remove bool
	}{
		{name: "apply", apply: true, owned: true, remove: true},
		{name: "dry run", owned: true},
		{name: "recent child", apply: true, recent: true, owned: true},
		{name: "held entry", apply: true, owned: true, heldRoot: true},
		{name: "held child", apply: true, owned: true, heldFile: true},
		{name: "other owner", apply: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "entry")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(dir, "file")
			if err := os.WriteFile(file, []byte("data"), 0o600); err != nil {
				t.Fatal(err)
			}
			old := now.Add(-12 * time.Hour)
			if !tt.recent {
				if err := os.Chtimes(file, old, old); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chtimes(dir, old, old); err != nil {
				t.Fatal(err)
			}
			held := map[string]struct{}{}
			if tt.heldRoot {
				held[dir] = struct{}{}
			}
			if tt.heldFile {
				held[file] = struct{}{}
			}
			deps := shmDeps{
				now:   func() time.Time { return now },
				owned: func(fs.FileInfo) bool { return tt.owned },
				held:  func() (map[string]struct{}, error) { return held, nil },
			}
			var output bytes.Buffer
			if err := pruneShm(t.Context(), root, 6*time.Hour, tt.apply, &printer{w: &output}, deps); err != nil {
				t.Fatal(err)
			}
			_, err := os.Lstat(dir)
			if tt.remove && !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("entry still exists: %v", err)
			}
			if !tt.remove && err != nil {
				t.Fatalf("entry missing: %v", err)
			}
		})
	}
}

func TestPruneShmSymlink(t *testing.T) {
	root, target := t.TempDir(), t.TempDir()
	file := filepath.Join(target, "file")
	if err := os.WriteFile(file, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	deps := shmDeps{
		now:   func() time.Time { return time.Now().Add(24 * time.Hour) },
		owned: func(fs.FileInfo) bool { return true },
		held:  func() (map[string]struct{}, error) { return nil, nil },
	}
	var output bytes.Buffer
	if err := pruneShm(t.Context(), root, 6*time.Hour, true, &printer{w: &output}, deps); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("link exists: %v", err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("target missing: %v", err)
	}
}

func TestShmOnlySkipsWorkspaces(t *testing.T) {
	var out, stderr bytes.Buffer
	root := t.TempDir()
	code := runIsolated(t, []string{"-shm-only", "-shm-root", root, "-root", filepath.Join(root, "missing"), "-to", testRecipient}, &out, &stderr)
	if code != 0 {
		t.Fatalf("run returned %d: %s", code, stderr.String())
	}
	if strings.Contains(out.String(), "go cache") || !strings.Contains(out.String(), "shm:") {
		t.Fatalf("unexpected report: %s", out.String())
	}
	if !strings.Contains(out.String(), "shm root "+root) || strings.Contains(out.String(), "roots [") {
		t.Fatalf("shm-only header does not show shm root: %s", out.String())
	}
}

func TestPruneShmApplyRechecksHeldPaths(t *testing.T) {
	root := t.TempDir()
	entry := filepath.Join(root, "entry")
	if err := os.WriteFile(entry, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-12 * time.Hour)
	if err := os.Chtimes(entry, old, old); err != nil {
		t.Fatal(err)
	}
	var scans int
	deps := shmDeps{
		now:   time.Now,
		owned: func(fs.FileInfo) bool { return true },
		held: func() (map[string]struct{}, error) {
			scans++
			if scans == 1 {
				return map[string]struct{}{}, nil
			}
			return map[string]struct{}{entry: {}}, nil
		},
	}
	if err := pruneShm(t.Context(), root, 6*time.Hour, true, &printer{w: &bytes.Buffer{}}, deps); err != nil {
		t.Fatal(err)
	}
	if scans != 2 {
		t.Fatalf("held path scans = %d, want initial and apply-time scans", scans)
	}
	if _, err := os.Stat(entry); err != nil {
		t.Fatalf("entry removed after becoming held: %v", err)
	}
}

func TestPruneShmKeepsOtherOwnerChild(t *testing.T) {
	root := t.TempDir()
	entry := filepath.Join(root, "entry")
	if err := os.Mkdir(entry, 0o700); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(entry, "other-owner")
	if err := os.WriteFile(child, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-12 * time.Hour)
	if err := os.Chtimes(child, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(entry, old, old); err != nil {
		t.Fatal(err)
	}
	deps := shmDeps{
		now: time.Now,
		owned: func(info fs.FileInfo) bool {
			return info.Name() != "other-owner"
		},
		held: func() (map[string]struct{}, error) { return nil, nil },
	}
	if err := pruneShm(t.Context(), root, 6*time.Hour, true, &printer{w: &bytes.Buffer{}}, deps); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(child); err != nil {
		t.Fatalf("other-owner child removed: %v", err)
	}
}

func TestPruneShmResolvesRootSymlink(t *testing.T) {
	root, target := t.TempDir(), t.TempDir()
	entry := filepath.Join(target, "old")
	if err := os.WriteFile(entry, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-12 * time.Hour)
	if err := os.Chtimes(entry, old, old); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "shm")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := pruneShm(t.Context(), link, 6*time.Hour, true, &printer{w: &bytes.Buffer{}}, shmDeps{
		now:   time.Now,
		owned: func(fs.FileInfo) bool { return true },
		held:  func() (map[string]struct{}, error) { return nil, nil },
	}); err != nil {
		t.Fatalf("prune through symlink root: %v", err)
	}
	if _, err := os.Stat(entry); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("entry inside resolved root was not pruned: %v", err)
	}
}

func TestPruneShmScanFailure(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "old")
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-12 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	scanErr := errors.New("scan failed")
	deps := shmDeps{
		now:   time.Now,
		owned: func(fs.FileInfo) bool { return true },
		held:  func() (map[string]struct{}, error) { return nil, scanErr },
	}
	var output bytes.Buffer
	if err := pruneShm(t.Context(), root, 6*time.Hour, true, &printer{w: &output}, deps); !errors.Is(err, scanErr) {
		t.Fatalf("got %v, want scan failure", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("entry removed after scan failure: %v", err)
	}
}
