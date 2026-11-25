// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	cases := []struct {
		name       string
		prepare    func(*testing.T, string)
		args       func(string) []string
		wantCode   int
		wantStdout string
		wantStderr string
		stderrPart string
	}{
		{
			name: "broken relative link",
			prepare: func(t *testing.T, root string) {
				writeCLIFile(t, root, "docs-bundle.toml", "exclude = []\n\n[[sections]]\nid = \"docs\"\nlabel = \"Docs\"\nroute = \"/docs\"\nprefix = \"docs/\"\n")
				writeCLIFile(t, root, "docs/index.md", "[missing](missing.md)\n")
			},
			args:     func(root string) []string { return []string{"check", "--root", root, "--config", "required"} },
			wantCode: 1, stderrPart: "docs-bundle: 1 broken link(s)\n  docs/index.md -> docs/missing.md\n",
		},
		{
			name: "route collision",
			prepare: func(t *testing.T, root string) {
				writeCLIFile(t, root, "docs-bundle.toml", "exclude = []\n\n[[sections]]\nid = \"one\"\nlabel = \"One\"\nroute = \"/same\"\nprefix = \"docs/first/\"\n\n[[sections]]\nid = \"two\"\nlabel = \"Two\"\nroute = \"/same\"\nprefix = \"docs/second/\"\n")
				writeCLIFile(t, root, "docs/first/readme.md", "# First\n")
				writeCLIFile(t, root, "docs/second/readme.md", "# Second\n")
			},
			args:     func(root string) []string { return []string{"check", "--root", root, "--config", "required"} },
			wantCode: 1, stderrPart: "docs-bundle: 2 page(s) collide on route(s): /same/readme\n",
		},
		{
			name: "broken link through a file",
			prepare: func(t *testing.T, root string) {
				writeCLIFile(t, root, "docs-bundle.toml", "exclude = []\n\n[[sections]]\nid = \"docs\"\nlabel = \"Docs\"\nroute = \"/docs\"\nprefix = \"docs/\"\n")
				writeCLIFile(t, root, "docs/index.md", "[x](a.md/sub.md)\n")
				writeCLIFile(t, root, "docs/a.md", "not a directory")
			},
			args:       func(root string) []string { return []string{"check", "--root", root, "--config", "required"} },
			wantCode:   1,
			stderrPart: "docs/index.md -> docs/a.md/sub.md",
		},
		{
			name:     "required config missing",
			prepare:  func(t *testing.T, root string) {},
			args:     func(root string) []string { return []string{"check", "--root", root, "--config", "required"} },
			wantCode: 2, stderrPart: "docs-bundle: configuration error: required publish config is missing:",
		},
		{
			name: "successful check",
			prepare: func(t *testing.T, root string) {
				writeCLIFile(t, root, "docs-bundle.toml", "exclude = []\n")
				writeCLIFile(t, root, "docs/index.md", "# Page\n")
			},
			args:     func(root string) []string { return []string{"check", "--root", root, "--config", "required"} },
			wantCode: 0, wantStdout: "docs-bundle: checked 1 page(s); no broken links\n",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			testCase.prepare(t, root)
			var stdout, stderr bytes.Buffer
			if got := run(testCase.args(root), &stdout, &stderr); got != testCase.wantCode {
				t.Fatalf("run() code = %d, want %d; stderr = %q", got, testCase.wantCode, stderr.String())
			}
			if got := stdout.String(); got != testCase.wantStdout {
				t.Errorf("stdout = %q, want %q", got, testCase.wantStdout)
			}
			if testCase.stderrPart != "" && !strings.Contains(stderr.String(), testCase.stderrPart) {
				t.Errorf("stderr = %q, want substring %q", stderr.String(), testCase.stderrPart)
			}
			if testCase.wantStderr != "" && stderr.String() != testCase.wantStderr {
				t.Errorf("stderr = %q, want %q", stderr.String(), testCase.wantStderr)
			}
		})
	}
}

func TestRunParseErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"missing command", nil, "expected the check subcommand"},
		{"wrong command", []string{"build"}, "expected the check subcommand"},
		{"unknown argument", []string{"check", "--verbose"}, "unknown argument \"--verbose\""},
		{"missing root value", []string{"check", "--root"}, "--root requires a value"},
		{"empty root value", []string{"check", "--root", ""}, "--root requires a value"},
		{"option as root value", []string{"check", "--root", "--config", "default"}, "--root requires a value"},
		{"duplicate root", []string{"check", "--root", "a", "--root", "b"}, "--root may appear once"},
		{"duplicate config", []string{"check", "--config", "default", "--config", "required"}, "--config may appear once"},
		{"invalid config", []string{"check", "--config", "other"}, "--config must be required or default"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(testCase.args, &stdout, &stderr); code != 2 {
				t.Fatalf("run() code = %d, want 2", code)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty", stdout.String())
			}
			if !strings.Contains(stderr.String(), "docs-bundle: "+testCase.want+"\nUsage: docs-bundle check [--root .] [--config required|default]\n") {
				t.Errorf("stderr = %q, want parser error %q", stderr.String(), testCase.want)
			}
		})
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

func TestPageErrorWriteFailure(t *testing.T) {
	cases := []struct {
		name   string
		config string
		files  []string
	}{
		{name: "unassigned page", config: "exclude = []\n[[sections]]\nid = \"a\"\nlabel = \"A\"\nroute = \"/a\"\nprefix = \"a/\"\n", files: []string{"other.md"}},
		{name: "route collision", config: "exclude = []\n[[sections]]\nid = \"one\"\nlabel = \"One\"\nroute = \"/same\"\nprefix = \"first/\"\n[[sections]]\nid = \"two\"\nlabel = \"Two\"\nroute = \"/same\"\nprefix = \"second/\"\n", files: []string{"first/readme.md", "second/readme.md"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeCLIFile(t, root, "docs-bundle.toml", tc.config)
			for _, file := range tc.files {
				writeCLIFile(t, root, file, "# Page\n")
			}
			var stdout, stderr bytes.Buffer
			args := []string{"check", "--root", root}
			if got := run(args, &stdout, &stderr); got != 1 {
				t.Fatalf("normal output: exit = %d, stderr = %q", got, stderr.String())
			}
			if got := run(args, &stdout, failingWriter{}); got != 2 {
				t.Fatalf("failed output: exit = %d, want 2", got)
			}
		})
	}
}

func writeCLIFile(t *testing.T, root, relative, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
