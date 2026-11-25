// SPDX-License-Identifier: MIT OR Apache-2.0

package bundle

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestFindBrokenLinksCases(t *testing.T) {
	cases := []struct {
		name   string
		source string
		files  []string
		pages  []BundlePage
		want   []BrokenLink
	}{
		{
			name:   "detects broken links and permits existing file",
			source: "[exists](asset.png) [missing](missing.md)\n",
			files:  []string{"docs/asset.png"},
			pages:  []BundlePage{{Repo: "acme/widgets", SourcePath: "docs/index.md", Route: "/docs"}},
			want:   []BrokenLink{{Repo: "acme/widgets", SourcePath: "docs/index.md", Target: "docs/missing.md"}},
		},
		{
			name:   "quoted reference description is not a definition",
			source: "[rule]: \"A quoted statement.\"\n",
			pages:  []BundlePage{{Repo: "acme/widgets", SourcePath: "index.md", Route: "/"}},
			want:   []BrokenLink{},
		},
		{
			name:   "ignores footnote definition",
			source: "[^1]: This is a footnote.\n",
			pages:  []BundlePage{{Repo: "acme/widgets", SourcePath: "index.md", Route: "/"}},
			want:   []BrokenLink{},
		},
		{
			name:   "references outside fences only",
			source: "[ref]: missing.md\n```\n[hidden]: ignored.md\n```\n",
			pages:  []BundlePage{{Repo: "acme/widgets", SourcePath: "index.md", Route: "/"}},
			want:   []BrokenLink{{Repo: "acme/widgets", SourcePath: "index.md", Target: "missing.md"}},
		},
		{
			name:   "fence closes before inline code masking",
			source: "```\n[hidden](hidden.md)\n```\n`[masked](masked.md)` [visible](visible.md)\n",
			pages:  []BundlePage{{Repo: "acme/widgets", SourcePath: "index.md", Route: "/"}},
			want:   []BrokenLink{{Repo: "acme/widgets", SourcePath: "index.md", Target: "visible.md"}},
		},
		{
			name:   "shorter fence does not close",
			source: "````\n```\n[h](h.md)\n````\n",
			pages:  []BundlePage{{Repo: "acme/widgets", SourcePath: "index.md", Route: "/"}},
			want:   []BrokenLink{},
		},
		{
			name:   "tab and indented fence do not close",
			source: "```\n\t\t\n    ```\n[h](h.md)\n```\n",
			pages:  []BundlePage{{Repo: "acme/widgets", SourcePath: "index.md", Route: "/"}},
			want:   []BrokenLink{},
		},
		{
			name:   "fence with trailing text does not close",
			source: "```\n```text\n[h](h.md)\n```\n",
			pages:  []BundlePage{{Repo: "acme/widgets", SourcePath: "index.md", Route: "/"}},
			want:   []BrokenLink{},
		},
		{
			name:   "titled nested image",
			source: "[![d](./d.png \"T\")](./d.png)",
			pages:  []BundlePage{{Repo: "acme/widgets", SourcePath: "docs/index.md", Route: "/"}},
			want: []BrokenLink{
				{Repo: "acme/widgets", SourcePath: "docs/index.md", Target: "docs/d.png"},
				{Repo: "acme/widgets", SourcePath: "docs/index.md", Target: "docs/d.png"},
			},
		},
		{
			name:   "titled link",
			source: "[h](h.md \"T\")",
			pages:  []BundlePage{{Repo: "acme/widgets", SourcePath: "docs/index.md", Route: "/"}},
			want:   []BrokenLink{{Repo: "acme/widgets", SourcePath: "docs/index.md", Target: "docs/h.md"}},
		},
		{
			name:   "encoded paths resolve to page routes and files",
			source: "[x](my%20doc.md)",
			files:  []string{"my doc.md"},
			pages:  []BundlePage{{Repo: "acme/widgets", SourcePath: "index.md", Route: "/"}, {Repo: "acme/widgets", SourcePath: "my doc.md", Route: "/my-doc"}},
			want:   []BrokenLink{},
		},
		{
			name:   "not a directory path is missing",
			source: "[x](a.md/sub.md)",
			files:  []string{"a.md"},
			pages:  []BundlePage{{Repo: "acme/widgets", SourcePath: "index.md", Route: "/"}},
			want:   []BrokenLink{{Repo: "acme/widgets", SourcePath: "index.md", Target: "a.md/sub.md"}},
		},
		{
			name:   "adjacent links",
			source: "[a](x.md)[b](y.md)",
			pages:  []BundlePage{{Repo: "acme/widgets", SourcePath: "docs/index.md", Route: "/"}},
			want: []BrokenLink{
				{Repo: "acme/widgets", SourcePath: "docs/index.md", Target: "docs/x.md"},
				{Repo: "acme/widgets", SourcePath: "docs/index.md", Target: "docs/y.md"},
			},
		},
		{
			name:   "adjacent linked images",
			source: "[![a](a.png)](x.md)[![b](b.png)](y.md)",
			pages:  []BundlePage{{Repo: "acme/widgets", SourcePath: "docs/index.md", Route: "/"}},
			want: []BrokenLink{
				{Repo: "acme/widgets", SourcePath: "docs/index.md", Target: "docs/a.png"},
				{Repo: "acme/widgets", SourcePath: "docs/index.md", Target: "docs/x.md"},
				{Repo: "acme/widgets", SourcePath: "docs/index.md", Target: "docs/b.png"},
				{Repo: "acme/widgets", SourcePath: "docs/index.md", Target: "docs/y.md"},
			},
		},
		{
			name:   "nested image ignores outer image target",
			source: "![x ![y](a.png) z](b.png)",
			files:  []string{"docs/a.png"},
			pages:  []BundlePage{{Repo: "acme/widgets", SourcePath: "docs/index.md", Route: "/"}},
			want:   []BrokenLink{},
		},
		{
			name:   "overlapping image and link target",
			source: "![a [b](x.png)",
			pages:  []BundlePage{{Repo: "acme/widgets", SourcePath: "docs/index.md", Route: "/"}},
			want:   []BrokenLink{{Repo: "acme/widgets", SourcePath: "docs/index.md", Target: "docs/x.png"}},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			writeFixture(t, root, testCase.pages[0].SourcePath, testCase.source)
			for _, file := range testCase.files {
				writeFixture(t, root, file, "present")
			}
			got, err := FindBrokenLinks(root, "acme/widgets", testCase.pages)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, testCase.want) {
				t.Errorf("FindBrokenLinks() = %#v, want %#v", got, testCase.want)
			}
		})
	}
	root := t.TempDir()
	writeFixture(t, root, "index.md", "[x](../outside.md)")
	outside, err := FindBrokenLinks(root, "acme/widgets", []BundlePage{{Repo: "acme/widgets", SourcePath: "index.md", Route: "/"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(outside) != 1 || outside[0].Target != "../outside.md" {
		t.Fatalf("outside-root link result = %#v", outside)
	}
	if _, err := os.Stat(filepath.Join(root, "index.md")); err != nil {
		t.Fatal(err)
	}
}
