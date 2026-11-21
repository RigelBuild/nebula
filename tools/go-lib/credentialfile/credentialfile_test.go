package credentialfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRead(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "surrounded.txt"), []byte(" \n credential \t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bom.txt"), []byte("\ufeff\u00a0credential\u2028\ufeff"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nel.txt"), []byte("\u0085credential\u0085"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "whitespace.txt"), []byte(" \n\t "), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := trimECMAScriptWhitespace("\u0085credential\u0085"); got != "\u0085credential\u0085" {
		t.Fatalf("trimECMAScriptWhitespace() removed ECMAScript-excluded U+0085: %q", got)
	}
	if err := os.Mkdir(filepath.Join(root, "directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		path string
		want string
		ok   bool
	}{
		{name: "returns undefined for an undefined path"},
		{name: "returns undefined for a whitespace-only path", path: " \t "},
		{name: "returns the trimmed contents of a non-empty file", path: filepath.Join(root, "surrounded.txt"), want: "credential", ok: true},
		{name: "trims ECMAScript whitespace and BOM from a credential", path: filepath.Join(root, "bom.txt"), want: "credential", ok: true},
		{name: "preserves U+0085 not in ECMAScript trim set", path: filepath.Join(root, "nel.txt"), want: "\u0085credential\u0085", ok: true},
		{name: "returns undefined for a missing file", path: filepath.Join(root, "missing.txt")},
		{name: "returns undefined for a directory path", path: filepath.Join(root, "directory")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Read(tt.path)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("Read() = %q, %v, want %q, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}
