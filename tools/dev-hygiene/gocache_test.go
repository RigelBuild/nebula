package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPruneTree(t *testing.T) {
	t.Parallel()
	cutoff := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	stale := cutoff.Add(-time.Hour)
	fresh := cutoff.Add(time.Hour)

	cases := []struct {
		name        string
		files       map[string]time.Time
		apply       bool
		wantRemoved int
		wantLeft    []string
	}{
		{
			name:        "dry run counts stale files and deletes nothing",
			files:       map[string]time.Time{"a/old": stale, "fresh": fresh},
			apply:       false,
			wantRemoved: 1,
			wantLeft:    []string{"a/old", "fresh"},
		},
		{
			name:        "apply removes only stale files",
			files:       map[string]time.Time{"a/old": stale, "fresh": fresh},
			apply:       true,
			wantRemoved: 1,
			wantLeft:    []string{"fresh"},
		},
		{
			name:        "file exactly at the cutoff is kept",
			files:       map[string]time.Time{"edge": cutoff},
			apply:       true,
			wantRemoved: 0,
			wantLeft:    []string{"edge"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			for rel, mod := range tc.files {
				path := filepath.Join(root, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, mod, mod); err != nil {
					t.Fatal(err)
				}
			}
			removed, err := pruneTree(context.Background(), root, cutoff, tc.apply)
			if err != nil {
				t.Fatal(err)
			}
			if removed != tc.wantRemoved {
				t.Fatalf("removed = %d, want %d", removed, tc.wantRemoved)
			}
			for _, rel := range tc.wantLeft {
				if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
					t.Fatalf("expected %s to remain: %v", rel, err)
				}
			}
		})
	}
}

func TestPruneTreeMissingRootIsNotAnError(t *testing.T) {
	t.Parallel()
	removed, err := pruneTree(context.Background(), filepath.Join(t.TempDir(), "absent"), time.Now(), true)
	if err != nil {
		t.Fatalf("missing root returned error: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
}

// plantGoFile writes one go-build file at <root>/<id[:2]>/<id><suffix> with the given body and mtime.
func plantGoFile(t *testing.T, root, id, suffix, body string, mod time.Time) string {
	t.Helper()
	sub := filepath.Join(root, id[:2])
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sub, id+suffix)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
	return path
}

// plantGoAction writes an <actionID>-a whose body names outputID, as Go's DiskCache.put does.
func plantGoAction(t *testing.T, root, actionID, outputID string, mod time.Time) string {
	t.Helper()
	return plantGoFile(t, root, actionID, "-a", "v1 "+actionID+" "+outputID+"                    1 1\n", mod)
}

// plantGoOutputDir writes an <outputID>-d directory holding one file, as go run executables are.
func plantGoOutputDir(t *testing.T, root, outputID string, mod time.Time) string {
	t.Helper()
	d := filepath.Join(root, outputID[:2], outputID+"-d")
	if err := os.MkdirAll(filepath.Join(d, "bin"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "bin", "tool"), []byte("xxxx"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(d, mod, mod); err != nil {
		t.Fatal(err)
	}
	return d
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

// Action and output IDs are different hashes in different subdirs, as Go writes them.
const (
	actOld    = "aa000000000000000000000000000001"
	actMid    = "bb000000000000000000000000000002"
	actNew    = "cc000000000000000000000000000003"
	outShared = "11000000000000000000000000000009"
	outNew    = "22000000000000000000000000000008"
)

func TestCapGoBuild(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)

	t.Run("outputs named by output id are counted and the oldest items go first", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		// Two actions share one output; nothing pairs by the same id.
		aOld := plantGoAction(t, root, actOld, outShared, base.Add(-4*time.Hour))
		dShared := plantGoFile(t, root, outShared, "-d", strings.Repeat("x", 1000), base.Add(-3*time.Hour))
		aMid := plantGoAction(t, root, actMid, outShared, base.Add(-2*time.Hour))
		aNew := plantGoAction(t, root, actNew, outNew, base.Add(-1*time.Hour))
		dNew := plantGoFile(t, root, outNew, "-d", strings.Repeat("y", 1000), base.Add(-1*time.Hour))

		items, total, err := scanGoBuild(t.Context(), root)
		if err != nil {
			t.Fatal(err)
		}
		size, err := goBuildSize(root)
		if err != nil {
			t.Fatal(err)
		}
		// Regression: same-id pairing counted only the -a files and missed every -d byte.
		if len(items) != 5 || total != size {
			t.Fatalf("scan = %d items, %d bytes; want 5 items, %d bytes", len(items), total, size)
		}

		// A cap just under the size minus the oldest -a forces removing aOld then dShared.
		removed, freed, err := capGoBuild(t.Context(), root, size-200, true)
		if err != nil {
			t.Fatal(err)
		}
		if removed != 2 || freed < 1000 {
			t.Fatalf("removed = %d freed = %d, want 2 items and the shared output", removed, freed)
		}
		if exists(t, aOld) || exists(t, dShared) {
			t.Fatal("oldest items kept while over the cap")
		}
		for _, p := range []string{aMid, aNew, dNew} {
			if !exists(t, p) {
				t.Fatalf("newer item %s removed before the oldest", p)
			}
		}
	})

	t.Run("under the cap removes nothing", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		aOld := plantGoAction(t, root, actOld, outShared, base.Add(-3*time.Hour))
		dShared := plantGoFile(t, root, outShared, "-d", "x", base.Add(-3*time.Hour))
		removed, _, err := capGoBuild(t.Context(), root, 1<<30, true)
		if err != nil {
			t.Fatal(err)
		}
		if removed != 0 || !exists(t, aOld) || !exists(t, dShared) {
			t.Fatalf("removed = %d under the cap, want 0", removed)
		}
	})

	t.Run("root files, foreign names and symlinks are never items", func(t *testing.T) {
		t.Parallel()
		base2 := t.TempDir()
		root := filepath.Join(base2, "go-build")
		canary := filepath.Join(base2, "canary.txt")
		if err := os.WriteFile(canary, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		d := plantGoOutputDir(t, root, outShared, base.Add(-3*time.Hour))
		if err := os.Symlink(canary, filepath.Join(d, "escape")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(canary, filepath.Join(root, outShared[:2], "link-d")); err != nil {
			t.Fatal(err)
		}
		readme := filepath.Join(root, "README")
		trim := filepath.Join(root, "trim.txt")
		for _, p := range []string{readme, trim} {
			if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := capGoBuild(t.Context(), root, 1, true); err != nil {
			t.Fatal(err)
		}
		if exists(t, d) {
			t.Fatal("-d directory holding a symlink was not removed")
		}
		for _, p := range []string{readme, trim, filepath.Join(root, outShared[:2], "link-d")} {
			if !exists(t, p) {
				t.Fatalf("%s was removed", p)
			}
		}
		got, err := os.ReadFile(canary)
		if err != nil || string(got) != "keep" {
			t.Fatalf("canary outside the cache changed: %q %v", got, err)
		}
	})

	t.Run("dry run removes nothing", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		aOld := plantGoAction(t, root, actOld, outShared, base.Add(-3*time.Hour))
		d := plantGoOutputDir(t, root, outShared, base.Add(-time.Hour))
		removed, _, err := capGoBuild(t.Context(), root, 1, false)
		if err != nil {
			t.Fatal(err)
		}
		if removed != 2 || !exists(t, aOld) || !exists(t, d) {
			t.Fatalf("dry run removed = %d or deleted an item", removed)
		}
	})
}

func TestPruneGoCachesCapsOnlyGoBuildAfterAgePass(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	cutoff := now.Add(-24 * time.Hour)
	root := filepath.Join(cacheDir, goBuildDir)
	stale := plantGoAction(t, root, actOld, outShared, cutoff.Add(-time.Hour))
	dShared := plantGoFile(t, root, outShared, "-d", strings.Repeat("x", 1000), now.Add(-3*time.Hour))
	aNew := plantGoAction(t, root, actNew, outNew, now.Add(-time.Hour))
	dNew := plantGoFile(t, root, outNew, "-d", strings.Repeat("y", 1000), now.Add(-time.Hour))
	lint := filepath.Join(cacheDir, "golangci-lint", "big")
	if err := os.MkdirAll(filepath.Dir(lint), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lint, []byte(strings.Repeat("z", 5000)), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := pruneGoCaches(t.Context(), cacheDir, cutoff, 1500, true, &printer{w: &buf}); err != nil {
		t.Fatal(err)
	}
	if exists(t, stale) {
		t.Fatal("age pass kept a stale -a")
	}
	if exists(t, dShared) {
		t.Fatal("cap kept the oldest -d while over the cap")
	}
	if !exists(t, aNew) || !exists(t, dNew) {
		t.Fatal("cap removed the newest items")
	}
	if !exists(t, lint) {
		t.Fatal("cap reached a cache other than go-build")
	}
	if !strings.Contains(buf.String(), "go cache go-build: 1 item(s) over the 1500-byte cap") {
		t.Fatalf("report = %q", buf.String())
	}
}

func TestPruneGoCachesJoinsCapError(t *testing.T) {
	t.Parallel()
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	cacheDir := t.TempDir()
	root := filepath.Join(cacheDir, goBuildDir)
	plantGoAction(t, root, actOld, outShared, time.Now())
	sub := filepath.Join(root, actOld[:2])
	if err := os.Chmod(sub, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(sub, 0o750); err != nil {
			t.Errorf("restore mode: %v", err)
		}
	})
	err := pruneGoCaches(t.Context(), cacheDir, time.Now().Add(-time.Hour), 1, true, &printer{w: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "cap go-build") {
		t.Fatalf("err = %v, want a joined cap go-build error", err)
	}
}

// goBuildSize sums every regular file under root, so a test can cap the cache just below its size.
func goBuildSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}
