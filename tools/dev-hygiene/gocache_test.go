package main

import (
	"context"
	"os"
	"path/filepath"
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
