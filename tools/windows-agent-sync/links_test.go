package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
)

// exampleLinkTable is the link table fixture. A real run reads the table that
// the layout's linkTable names; the tests read this example instead.
const exampleLinkTable = "testdata/links.cue"

// exampleAgentConfig is the Home Manager fixture the link table must match.
const exampleAgentConfig = "testdata/agent-config.nix"

// linkAgentEntry matches `"<dst>".source = linkAgent "<src>"` and
// `"<dst>" = { source = linkAgent "<src>"; ... }` in agent-config.nix.
var linkAgentEntry = regexp.MustCompile(`"([^"]+)"(?:\.source\s*=|\s*=\s*\{[^}]*?source\s*=)\s*linkAgent\s+"([^"]+)"`)

func TestLoadLinksExample(t *testing.T) {
	got, err := loadLinks(exampleLinkTable)
	if err != nil {
		t.Fatal(err)
	}
	want := []link{
		{Src: "example/agents/AGENTS.md", Dst: ".agents/AGENTS.md"},
		{Src: "example/agents/rules", Dst: ".agents/rules"},
		{Src: "example/agents/skills", Dst: ".agents/skills"},
		{Src: "example/agents/cotal", Dst: ".cotal-config"},
		{Src: "example/agents/extensions", Dst: ".omp/agent/extensions"},
		{Src: "example/agents/autolearn", Dst: ".omp/agent/autolearn"},
		{Src: "example/agents/agents", Dst: ".omp/agent/agents"},
		{Src: "example/agents/config.yml", Dst: ".omp/agent/config.yml"},
		{Src: "example/agents/models.yml", Dst: ".omp/agent/models.yml"},
		{Src: "example/dotfiles/jj/config.toml", Dst: "%APPDATA%/jj/config.toml"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("loadLinks() = %v\nwant %v", got, want)
	}
}

func TestLinkTableMatchesAgentConfig(t *testing.T) {
	data, err := os.ReadFile(exampleAgentConfig)
	if err != nil {
		t.Fatal(err)
	}
	want := make(map[string]string)
	for _, match := range linkAgentEntry.FindAllStringSubmatch(string(data), -1) {
		dst, src := match[1], match[2]
		if src == "mcp.json" {
			continue
		}
		want[dst] = "example/agents/" + src
	}
	if len(want) == 0 {
		t.Fatalf("no linkAgent entries parsed from %s", exampleAgentConfig)
	}
	table, err := loadLinks(exampleLinkTable)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]string)
	for _, managed := range table {
		if managed.Dst == "%APPDATA%/jj/config.toml" {
			continue
		}
		got[managed.Dst] = managed.Src
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("link table drifts from %s:\n got  %v\n want %v", exampleAgentConfig, got, want)
	}
}

func TestLoadLinksRejectsInvalid(t *testing.T) {
	for _, tc := range []struct{ name, input string }{
		{"empty source", `links: [{src: "", dst: ".x"}]`},
		{"empty destination", `links: [{src: "x", dst: ""}]`},
		{"unknown field", `links: [{src: "x", dst: ".x", extra: true}]`},
		{"source escapes repo root", `links: [{src: "../../etc/passwd", dst: ".x"}]`},
		{"destination escapes profile", `links: [{src: "x", dst: "../../evil"}]`},
		{"absolute destination", `links: [{src: "x", dst: "/etc/passwd"}]`},
		{"appdata destination escapes", `links: [{src: "x", dst: "%APPDATA%/../../evil"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "links.cue")
			if err := os.WriteFile(path, []byte(tc.input), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadLinks(path); err == nil {
				t.Fatal("loadLinks() succeeded on an invalid link table")
			}
		})
	}
}

func TestSyncLinks(t *testing.T) {
	root := t.TempDir()
	co := checkout{root: filepath.Join(root, "repo")}
	home := filepath.Join(root, "home")
	appData := filepath.Join(root, "appdata")
	table, err := loadLinks(exampleLinkTable)
	if err != nil {
		t.Fatal(err)
	}
	fs := &countingLinkFS{linkFS: osLinkFS{}}
	if len(table) == 0 {
		t.Fatal("link table is empty")
	}
	if err := syncLinks(co, home, appData, table, fs); err != nil {
		t.Fatal(err)
	}
	if fs.symlinks != len(table) {
		t.Fatalf("created links = %d, want %d", fs.symlinks, len(table))
	}
	for _, managed := range table {
		source, err := filepath.Abs(co.path(managed.Src))
		if err != nil {
			t.Fatal(err)
		}
		target, err := linkTarget(home, appData, managed.Dst)
		if err != nil {
			t.Fatal(err)
		}
		gotTarget, err := os.Readlink(target)
		if err != nil || !sameLinkTarget(gotTarget, source) {
			t.Fatalf("link %q targets %q, %v; want %q", managed.Dst, gotTarget, err, source)
		}
	}
	if err := syncLinks(co, home, appData, table, fs); err != nil {
		t.Fatalf("second syncLinks(): %v", err)
	}
	if fs.symlinks != len(table) {
		t.Fatalf("second sync created links = %d, want %d", fs.symlinks, len(table))
	}
}

func TestSyncLink(t *testing.T) {
	tests := []struct {
		name        string
		initial     string
		initialLink bool
		wrongLink   bool
		wantBackup  bool
		wantRemove  int
		wantSymlink int
		wantRename  int
	}{
		{name: "backup non-link", initial: "user config", wantBackup: true, wantRename: 1, wantSymlink: 1},
		{name: "missing target", wantSymlink: 1},
		{name: "replace wrong symlink", wrongLink: true, wantRemove: 1, wantSymlink: 1},
		{name: "leave correct symlink untouched", initialLink: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testSyncLinkCase(t, test)
		})
	}
}

func testSyncLinkCase(t *testing.T, test struct {
	name        string
	initial     string
	initialLink bool
	wrongLink   bool
	wantBackup  bool
	wantRemove  int
	wantSymlink int
	wantRename  int
}) {
	t.Helper()
	dir := t.TempDir()
	target := filepath.Join(dir, "config")
	source := filepath.Join(dir, "source")
	wrongSource := filepath.Join(dir, "wrong-source")
	if err := os.WriteFile(source, []byte("managed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if test.initial != "" {
		if err := os.WriteFile(target, []byte(test.initial), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if test.initialLink || test.wrongLink {
		linkTarget := source
		if test.wrongLink {
			linkTarget = wrongSource
		}
		if err := os.Symlink(linkTarget, target); err != nil {
			t.Fatal(err)
		}
	}
	fs := &countingLinkFS{linkFS: osLinkFS{}}
	if err := syncLink(fs, source, target); err != nil {
		t.Fatal(err)
	}
	checkLinkBackup(t, target, test.wantBackup, test.initial)
	gotTarget, err := os.Readlink(target)
	if err != nil || !sameLinkTarget(gotTarget, source) {
		t.Fatalf("link target = %q, %v", gotTarget, err)
	}
	if fs.removes != test.wantRemove || fs.symlinks != test.wantSymlink || fs.renames != test.wantRename {
		t.Fatalf("link operations: remove=%d symlink=%d rename=%d", fs.removes, fs.symlinks, fs.renames)
	}
	if err := syncLink(fs, source, target); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if test.initialLink && (fs.removes != 0 || fs.symlinks != 0 || fs.renames != 0) {
		t.Fatalf("correct link was modified: remove=%d symlink=%d rename=%d", fs.removes, fs.symlinks, fs.renames)
	}
}

func checkLinkBackup(t *testing.T, target string, wantBackup bool, initial string) {
	t.Helper()
	backup, err := os.ReadFile(target + ".hm-backup")
	if wantBackup {
		if err != nil || string(backup) != initial {
			t.Fatalf("backup = %q, %v", backup, err)
		}
		return
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected backup read error = %v", err)
	}
}

type countingLinkFS struct {
	linkFS
	removes  int
	symlinks int
	renames  int
}

func (fs *countingLinkFS) Remove(name string) error {
	fs.removes++
	return fs.linkFS.Remove(name)
}

func (fs *countingLinkFS) Symlink(oldname, newname string) error {
	fs.symlinks++
	return fs.linkFS.Symlink(oldname, newname)
}

func (fs *countingLinkFS) Rename(oldpath, newpath string) error {
	fs.renames++
	return fs.linkFS.Rename(oldpath, newpath)
}
