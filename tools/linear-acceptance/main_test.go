package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRewrite(t *testing.T) {
	dir := t.TempDir()
	writeFile := func(name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	passing := writeFile("passing.md", "## Acceptance criteria\n\n- [ ] merged PR #123\n")
	checkedOnly := writeFile("checked.md", "## Acceptance criteria\n- [x] already done\n")
	upperChecked := writeFile("upper.md", "## Acceptance criteria\n- [X] already done\n")
	laterHeading := writeFile("later.md", "## Acceptance criteria\nprose only\n## Notes\n- [ ] stray item\n")
	fencedItem := writeFile("fenced.md", "## Acceptance criteria\n```md\n- [ ] example only\n```\n")
	missing := filepath.Join(dir, "missing.md")
	const inlinePassing = "## Acceptance criteria\n- [ ] merged PR #123"
	const inlineFenced = "## Acceptance criteria\n```\n- [ ] in fence\n```\n- [ ] real item"
	const inlineMixed = "## Acceptance criteria\n- [X] done\n- [ ] open"

	cases := []struct {
		name       string
		args       []string
		want       []string
		wantErr    string
		wantNotice string
	}{
		{
			name: "passes with an unchecked item under the heading",
			args: []string{"issue", "create", "-t", "T", "--description-file", passing},
			want: []string{"issue", "create", "-t", "T", "--description-file", passing},
		},
		{
			name: "passes with the --description-file= form",
			args: []string{"issue", "create", "--description-file=" + passing},
			want: []string{"issue", "create", "--description-file=" + passing},
		},
		{
			name: "passes with inline -d text",
			args: []string{"issue", "create", "-d", inlinePassing},
			want: []string{"issue", "create", "-d", inlinePassing},
		},
		{
			name: "passes with the --description= form",
			args: []string{"issue", "create", "--description=" + inlinePassing},
			want: []string{"issue", "create", "--description=" + inlinePassing},
		},
		{
			name: "passes with the -d= form",
			args: []string{"issue", "create", "-d=" + inlinePassing},
			want: []string{"issue", "create", "-d=" + inlinePassing},
		},
		{
			name: "passes an unchecked item after an upper-case checked item",
			args: []string{"issue", "create", "-d", inlineMixed},
			want: []string{"issue", "create", "-d", inlineMixed},
		},
		{
			name: "passes an unchecked item after a closed code fence",
			args: []string{"issue", "create", "-d", inlineFenced},
			want: []string{"issue", "create", "-d", inlineFenced},
		},
		{
			name:    "refuses when there is no description",
			args:    []string{"issue", "create", "-t", "T"},
			wantErr: `description has no "## Acceptance criteria" heading`,
		},
		{
			name:    "refuses a description with no heading",
			args:    []string{"issue", "create", "-d", "plain text, no heading"},
			wantErr: `description has no "## Acceptance criteria" heading`,
		},
		{
			name:    "refuses a heading with only lower-case checked items",
			args:    []string{"issue", "create", "--description-file", checkedOnly},
			wantErr: `"## Acceptance criteria" has no "- [ ]" item`,
		},
		{
			name:    "refuses a heading with only upper-case checked items",
			args:    []string{"issue", "create", "--description-file", upperChecked},
			wantErr: `"## Acceptance criteria" has no "- [ ]" item`,
		},
		{
			name:    "refuses an item under a later heading only",
			args:    []string{"issue", "create", "--description-file", laterHeading},
			wantErr: `"## Acceptance criteria" has no "- [ ]" item`,
		},
		{
			name:    "refuses an unchecked item inside a code fence",
			args:    []string{"issue", "create", "--description-file", fencedItem},
			wantErr: `"## Acceptance criteria" has no "- [ ]" item`,
		},
		{
			name:    "refuses a description file that cannot be read",
			args:    []string{"issue", "create", "--description-file", missing},
			wantErr: "cannot read description file " + missing,
		},
		{
			name:    "refuses a value option with no value token",
			args:    []string{"issue", "create", "-t"},
			wantErr: "-t needs a value",
		},
		{
			name:       "escape with a reason strips the flag and its value",
			args:       []string{"issue", "create", "-t", "T", "--no-acceptance-criteria", "ad hoc spike"},
			want:       []string{"issue", "create", "-t", "T"},
			wantNotice: "linear-acceptance: acceptance criteria waived: \"ad hoc spike\"\n",
		},
		{
			name:       "escape with the = form strips the flag",
			args:       []string{"issue", "create", "-t", "T", "--no-acceptance-criteria=ad hoc spike"},
			want:       []string{"issue", "create", "-t", "T"},
			wantNotice: "linear-acceptance: acceptance criteria waived: \"ad hoc spike\"\n",
		},
		{
			name:       "escape skips reading the description file",
			args:       []string{"issue", "create", "--description-file", missing, "--no-acceptance-criteria", "spike"},
			want:       []string{"issue", "create", "--description-file", missing},
			wantNotice: "linear-acceptance: acceptance criteria waived: \"spike\"\n",
		},
		{
			name:    "escape with an empty reason is refused",
			args:    []string{"issue", "create", "--description-file", passing, "--no-acceptance-criteria", ""},
			wantErr: "--no-acceptance-criteria needs a non-empty reason",
		},
		{
			name:    "escape with no reason argument is refused",
			args:    []string{"issue", "create", "-t", "T", "--no-acceptance-criteria"},
			wantErr: "--no-acceptance-criteria needs a non-empty reason",
		},
		{
			name: "i alias passes through with an unchecked item",
			args: []string{"i", "create", "-t", "T", "--description-file", passing},
			want: []string{"i", "create", "-t", "T", "--description-file", passing},
		},
		{
			name:    "i alias with no description is refused",
			args:    []string{"i", "create", "-t", "T"},
			wantErr: `description has no "## Acceptance criteria" heading`,
		},
		{
			name:    "i alias with a missing description file is refused",
			args:    []string{"i", "create", "-t", "T", "--description-file=" + missing},
			wantErr: "cannot read description file " + missing,
		},
		{
			name:    "root --workspace before issue does not skip the guard",
			args:    []string{"--workspace", "w", "issue", "create", "-t", "T"},
			wantErr: `description has no "## Acceptance criteria" heading`,
		},
		{
			name: "root --workspace= before issue passes with an unchecked item",
			args: []string{"--workspace=w", "issue", "create", "-d", inlinePassing},
			want: []string{"--workspace=w", "issue", "create", "-d", inlinePassing},
		},
		{
			name:    "issue-level --workspace before create does not skip the guard",
			args:    []string{"issue", "--workspace", "w", "create", "-t", "T", "--description-file=" + missing},
			wantErr: "cannot read description file " + missing,
		},
		{
			name:    "i alias with issue-level --workspace= is refused",
			args:    []string{"i", "--workspace=w", "create", "-t", "T"},
			wantErr: `description has no "## Acceptance criteria" heading`,
		},
		{
			name:    "bypass: -t= takes the next token, so --description-file=... is the title",
			args:    []string{"issue", "create", "-t=", "--description-file=" + passing},
			wantErr: `description has no "## Acceptance criteria" heading`,
		},
		{
			name:    "bypass: -d= takes the next token, so --description-file=... is the description",
			args:    []string{"issue", "create", "-t", "T", "-d=", "--description-file=" + passing},
			wantErr: `description has no "## Acceptance criteria" heading`,
		},
		{
			name: "--workspace= takes i as its slug, so create is not reached and the CLI fails with a usage error",
			args: []string{"--workspace=", "i", "create", "-t", "T"},
			want: []string{"--workspace=", "i", "create", "-t", "T"},
		},
		{
			name:    "bypass: --workspace= x takes x as its slug, so issue create is reached and the file is read",
			args:    []string{"--workspace=", "x", "issue", "create", "-t", "T", "--description-file=" + missing},
			wantErr: "cannot read description file " + missing,
		},
		{
			name:    "bypass: -tal splits to -t -a -l, so -l takes --description-file=... as its label",
			args:    []string{"issue", "create", "-tal", "--description-file=" + passing},
			wantErr: `description has no "## Acceptance criteria" heading`,
		},
		{
			name:    "-tal with a separate file path: -l takes --description-file as its label, no file read",
			args:    []string{"issue", "create", "-tal", "--description-file", passing},
			wantErr: `description has no "## Acceptance criteria" heading`,
		},
		{
			name:    "-dt makes -d take -t as its value, and a second description option is refused",
			args:    []string{"issue", "create", "-dt", "--description-file=" + passing},
			wantErr: "refuses two description options",
		},
		{
			name:    "bypass: -t takes --description-file=... as its title",
			args:    []string{"issue", "create", "-t", "--description-file=" + passing},
			wantErr: `description has no "## Acceptance criteria" heading`,
		},
		{
			name:    "bypass: -l takes --description-file=... as its label",
			args:    []string{"issue", "create", "-t", "T", "-l", "--description-file=" + passing},
			wantErr: `description has no "## Acceptance criteria" heading`,
		},
		{
			name:    "bypass: text after -- is not read as a flag",
			args:    []string{"issue", "create", "-t", "T", "--", "-d", inlinePassing},
			wantErr: `description has no "## Acceptance criteria" heading`,
		},
		{
			name:    "bypass: -d takes --help as its value, so help does not pass through",
			args:    []string{"issue", "create", "-t", "T", "-d", "--help"},
			wantErr: `description has no "## Acceptance criteria" heading`,
		},
		{
			name: "false refusal: -t takes -d as its title, the next flag reads the file",
			args: []string{"issue", "create", "-t", "-d", "--description-file", passing},
			want: []string{"issue", "create", "-t", "-d", "--description-file", passing},
		},
		{
			name: "--help as a -t value is not help",
			args: []string{"issue", "create", "-t", "--help", "-d", inlinePassing},
			want: []string{"issue", "create", "-t", "--help", "-d", inlinePassing},
		},
		{
			name: "non-create subcommand passes untouched",
			args: []string{"issue", "view", "RIG-1", "--no-acceptance-criteria", "x"},
			want: []string{"issue", "view", "RIG-1", "--no-acceptance-criteria", "x"},
		},
		{
			name: "help on create passes untouched",
			args: []string{"issue", "create", "--help"},
			want: []string{"issue", "create", "--help"},
		},
		{
			name: "help after --workspace on create passes untouched",
			args: []string{"--workspace", "w", "issue", "create", "-h"},
			want: []string{"--workspace", "w", "issue", "create", "-h"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var notice bytes.Buffer
			got, err := rewrite(tc.args, &notice)
			if notice.String() != tc.wantNotice {
				t.Fatalf("notice = %q, want %q", notice.String(), tc.wantNotice)
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("rewrite() error = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("rewrite() error = %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("rewrite() = %q, want %q", got, tc.want)
			}
		})
	}
}
