package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestReplaceBinaryRemovesStaleBackupBeforeMovingTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "tool.exe")
	temp := filepath.Join(dir, "tool.tmp")
	old := target + ".old"
	for path, content := range map[string]string{target: "current", temp: "replacement", old: "stale"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var renames [][2]string
	rename := func(src, dst string) error {
		if _, err := os.Lstat(dst); err == nil {
			return errors.New("rename destination already exists")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		renames = append(renames, [2]string{src, dst})
		return os.Rename(src, dst)
	}
	if err := replaceBinaryWithRename(temp, target, rename); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(renames, [][2]string{{target, old}, {temp, target}}) {
		t.Fatalf("renames = %v", renames)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "replacement" {
		t.Fatalf("target = %q, %v", got, err)
	}
	got, err = os.ReadFile(old)
	if err != nil || string(got) != "current" {
		t.Fatalf("backup = %q, %v", got, err)
	}
}

func TestReplaceBinaryLeavesTargetWhenStaleBackupRemovalFails(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "tool.exe")
	temp := filepath.Join(dir, "tool.tmp")
	old := target + ".old"
	if err := os.WriteFile(target, []byte("current"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(temp, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(old, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "locked"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	renamed := false
	err := replaceBinaryWithRename(temp, target, func(string, string) error {
		renamed = true
		return nil
	})
	if err == nil {
		t.Fatal("replaceBinaryWithRename() succeeded with an unremovable backup")
	}
	if renamed {
		t.Fatal("rename ran after stale backup removal failed")
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "current" {
		t.Fatalf("target = %q, %v", got, err)
	}
}

func TestReplaceBinaryRestoresTargetWhenInstallRenameFails(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "tool.exe")
	temp := filepath.Join(dir, "tool.tmp")
	old := target + ".old"
	if err := os.WriteFile(target, []byte("current"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(temp, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	rename := func(src, dst string) error {
		if src == temp && dst == target {
			return errors.New("install rename failed")
		}
		return os.Rename(src, dst)
	}
	if err := replaceBinaryWithRename(temp, target, rename); err == nil {
		t.Fatal("replaceBinaryWithRename() succeeded after install rename failed")
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "current" {
		t.Fatalf("restored target = %q, %v", got, err)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backup remains after restore: %v", err)
	}
}

func TestInstallPins(t *testing.T) {
	for _, test := range []struct {
		name          string
		initial       string
		initialOld    string
		content       string
		sha           string
		wantError     bool
		wantDownloads int
		wantInstalled string
		wantOld       string
	}{
		{name: "skip on match", initial: "current", content: "replacement", sha: hashText("current"), wantInstalled: "current"},
		{name: "install verified pin", content: "downloaded", sha: hashText("downloaded"), wantDownloads: 1, wantInstalled: "downloaded"},
		{name: "backup prior binary", initial: "old", content: "new", sha: hashText("new"), wantDownloads: 1, wantInstalled: "new", wantOld: "old"},
		{name: "replace prior backup", initial: "old", initialOld: "older", content: "new", sha: hashText("new"), wantDownloads: 1, wantInstalled: "new", wantOld: "old"},
		{name: "SHA mismatch leaves current binary", initial: "current", initialOld: "older", content: "bad", sha: hashText("expected"), wantError: true, wantDownloads: 1, wantInstalled: "current", wantOld: "older"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "omp.exe")
			if test.initial != "" {
				if err := os.WriteFile(target, []byte(test.initial), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if test.initialOld != "" {
				if err := os.WriteFile(target+".old", []byte(test.initialOld), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			downloads := 0
			get := func(_ context.Context, _ string) (io.ReadCloser, error) {
				downloads++
				return io.NopCloser(strings.NewReader(test.content)), nil
			}
			pin := Pin{URL: "https://example.test/omp", SHA256: test.sha, File: "omp.exe"}
			err := installPins(context.Background(), get, map[string]Pin{"omp": pin}, dir)
			if (err != nil) != test.wantError {
				t.Fatalf("installPins() error = %v, wantError %v", err, test.wantError)
			}
			if downloads != test.wantDownloads {
				t.Fatalf("downloads = %d, want %d", downloads, test.wantDownloads)
			}
			installed, readErr := os.ReadFile(target)
			if readErr != nil || string(installed) != test.wantInstalled {
				t.Fatalf("installed = %q, %v", installed, readErr)
			}
			old, oldErr := os.ReadFile(target + ".old")
			if test.wantOld != "" && (oldErr != nil || string(old) != test.wantOld) {
				t.Fatalf("old binary = %q, %v", old, oldErr)
			}
			temps, err := filepath.Glob(filepath.Join(dir, ".pin-*.tmp"))
			if err != nil || len(temps) != 0 {
				t.Fatalf("leftover temp files = %v, %v", temps, err)
			}
		})
	}
}

func TestInstallPinRejectsInvalidInputs(t *testing.T) {
	for _, pin := range []Pin{
		{URL: "https://example.test/omp", SHA256: "bad", File: "omp.exe"},
		{URL: "https://example.test/omp", SHA256: hashText("valid"), File: "../omp.exe"},
	} {
		if err := installPins(context.Background(), func(context.Context, string) (io.ReadCloser, error) {
			t.Fatal("invalid pin triggered a download")
			return nil, errors.New("unexpected download")
		}, map[string]Pin{"invalid": pin}, t.TempDir()); err == nil {
			t.Fatalf("installPins accepted invalid pin: %+v", pin)
		}
	}
}

func hashText(text string) string {
	hash := sha256.Sum256([]byte(text))
	return hex.EncodeToString(hash[:])
}

func TestReadPinsSplitsBuiltVersions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pins.json")
	const fixture = `{
	"omp": {"url": "https://example.test/omp.exe", "sha256": "aa", "file": "omp.exe"},
	"built": {"jj-hooks": "0.6.0", "linear-cli": "2.6.0"}
}`
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	pins, built, err := readPins(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, found := pins["built"]; found {
		t.Fatal("built key leaked into the binary pins")
	}
	if pins["omp"].File != "omp.exe" {
		t.Fatalf("omp pin = %+v", pins["omp"])
	}
	if built != (builtPins{JJHooks: "0.6.0", LinearCLI: "2.6.0"}) {
		t.Fatalf("built pins = %+v", built)
	}
}

func TestReadPinsRejectsUnpinnedBuiltTools(t *testing.T) {
	for _, test := range []struct {
		name    string
		fixture string
	}{
		{name: "no built key", fixture: `{"omp": {"url": "u", "sha256": "aa", "file": "omp.exe"}}`},
		{name: "empty jj-hooks", fixture: `{"built": {"jj-hooks": "", "linear-cli": "2.6.0"}}`},
		{name: "empty linear-cli", fixture: `{"built": {"jj-hooks": "0.6.0", "linear-cli": ""}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "pins.json")
			if err := os.WriteFile(path, []byte(test.fixture), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := readPins(path); err == nil {
				t.Fatal("readPins() accepted an unpinned built tool")
			}
		})
	}
}
