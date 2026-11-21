package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/rawcap"
)

const fakeRepo = "s3:fake/repo"

func TestRootCommandRegistersRawCap(t *testing.T) {
	command, _, err := newRootCmd().Find([]string{"rawcap"})
	if err != nil {
		t.Fatal(err)
	}
	if command.Name() != "rawcap" {
		t.Fatalf("command = %q, want rawcap", command.Name())
	}
	for _, flag := range []string{"repo", "state-dir", "cap-bytes"} {
		if command.Flags().Lookup(flag) == nil {
			t.Fatalf("rawcap command is missing --%s", flag)
		}
	}
}

// installFakeRestic puts a restic on PATH that checks its argv, then prints stats JSON and exits with code.
func installFakeRestic(t *testing.T, statsJSON string, code int) {
	t.Helper()
	binDir := t.TempDir()
	script := "#!/bin/sh\n" +
		"case \" $* \" in *' --repo " + fakeRepo + " '*) ;; *) echo missing repo >&2; exit 99;; esac\n" +
		"case \" $* \" in *' --password-file password '*) ;; *) echo missing password-file >&2; exit 99;; esac\n" +
		"printf '%s' '" + statsJSON + "'\n" +
		"exit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "restic"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// stateDirWithPassword returns a state dir holding a password file.
func stateDirWithPassword(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "password"), []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runRawCapArgs(stateDir string) error {
	command := newRootCmd()
	command.SetArgs([]string{"rawcap", "--repo", fakeRepo, "--state-dir", stateDir, "--cap-bytes", "100"})
	return command.Execute()
}

func TestRawCapCommandRefusesAtAndOverCap(t *testing.T) {
	cases := []struct {
		name     string
		stats    string
		wantOver bool
	}{
		{name: "under cap passes", stats: `{"total_size": 99}`},
		{name: "at cap refuses", stats: `{"total_size": 100}`, wantOver: true},
		{name: "over cap refuses", stats: `{"total_size": 101}`, wantOver: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			installFakeRestic(t, tc.stats, 0)
			err := runRawCapArgs(stateDirWithPassword(t))
			if tc.wantOver {
				if !errors.Is(err, rawcap.ErrOverCap) {
					t.Fatalf("Execute() error = %v, want ErrOverCap", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Execute() error = %v, want nil", err)
			}
		})
	}
}

func TestRawCapFirstRunPasses(t *testing.T) {
	t.Run("restic exit 10 passes", func(t *testing.T) {
		installFakeRestic(t, "", 10)
		if err := runRawCapArgs(stateDirWithPassword(t)); err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}
	})
	t.Run("missing password file passes", func(t *testing.T) {
		installFakeRestic(t, "", 1)
		if err := runRawCapArgs(t.TempDir()); err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}
	})
}

func TestRawCapFailsClosed(t *testing.T) {
	t.Run("other restic failure refuses", func(t *testing.T) {
		installFakeRestic(t, "", 1)
		if err := runRawCapArgs(stateDirWithPassword(t)); err == nil {
			t.Fatal("Execute() error = nil, want restic failure")
		}
	})
	t.Run("unreadable password refuses", func(t *testing.T) {
		installFakeRestic(t, `{"total_size": 1}`, 0)
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "password"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := runRawCapArgs(dir); err == nil {
			t.Fatal("Execute() error = nil, want password read failure")
		}
	})
}
