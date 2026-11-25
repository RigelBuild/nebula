// SPDX-License-Identifier: MIT OR Apache-2.0

package bundle

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCollectPagesCases(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		config  PublishConfig
		want    map[string]string
		wantErr string
	}{
		{
			name: "catch all and explicit files",
			files: map[string]string{
				"README.md": "# Page", "docs/README.md": "# Page", "apps/widget/README.md": "# Page",
				"tools/item/AGENTS.md": "# Page", "notes/guide.md": "# Page",
			},
			config: PublishConfig{
				Exclude:  []string{},
				Sections: []SectionRule{{ID: "contributing", Label: "Contributing", Route: "/contributing", Files: []string{"README.md", "docs/README.md"}}},
				Packages: &PackageRules{Route: "/packages", Allow: []string{"notes/"}, ReadmeDepth: 3, Nested: []string{"apps"}},
			},
			want: map[string]string{
				"README.md": "/contributing/readme", "docs/README.md": "/contributing/docs-readme",
				"apps/widget/README.md": "/packages/apps-widget/readme", "tools/item/AGENTS.md": "/packages/tools/item/agents",
				"notes/guide.md": "/packages/notes/guide",
			},
		},
		{
			name:    "reject stray package page",
			files:   map[string]string{"tools/item/stray.md": "# Stray"},
			config:  PublishConfig{Exclude: []string{}, Sections: []SectionRule{}, Packages: &PackageRules{Route: "/packages", Allow: []string{}, ReadmeDepth: 3}},
			wantErr: "stray package page: tools/item/stray.md",
		},
		{
			name:    "root readme not claimed by package",
			files:   map[string]string{"README.md": "# Root"},
			config:  PublishConfig{Exclude: []string{}, Sections: []SectionRule{}, Packages: &PackageRules{Route: "/packages", Allow: []string{}, ReadmeDepth: 3}},
			wantErr: "stray package page: README.md",
		},
		{
			name:   "default route paths",
			files:  map[string]string{"README.md": "# Widgets", "docs/guide.md": "# Guide"},
			config: PublishConfig{Exclude: []string{}, Sections: []SectionRule{}},
			want:   map[string]string{"README.md": "/acme/widgets/readme", "docs/guide.md": "/acme/widgets/docs/guide"},
		},
		{
			name:    "unassigned required page",
			files:   map[string]string{"docs/guidelines.md": "# Guidelines"},
			config:  PublishConfig{Exclude: []string{}, Sections: []SectionRule{{ID: "guide", Label: "Guide", Route: "/guide", Prefix: new("docs/guide")}}},
			wantErr: "unassigned page: docs/guidelines.md",
		},
		{
			name:   "section prefix without trailing slash",
			files:  map[string]string{"docs/guide/intro.md": "# Intro"},
			config: PublishConfig{Exclude: []string{}, Sections: []SectionRule{{ID: "guide", Label: "Guide", Route: "/guide", Prefix: new("docs/guide")}}},
			want:   map[string]string{"docs/guide/intro.md": "/guide/intro"},
		},
		{
			name:    "allowed README too deep",
			files:   map[string]string{"packages/widgets/a/b/README.md": "# Deep"},
			config:  PublishConfig{Exclude: []string{}, Sections: []SectionRule{}, Packages: &PackageRules{Route: "/packages", Allow: []string{"widgets"}, ReadmeDepth: 1}},
			wantErr: "stray package page: packages/widgets/a/b/README.md",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			for path, content := range testCase.files {
				writeFixture(t, root, path, content)
			}
			source := FetchedSource{BundleSource: BundleSource{Repo: "acme/widgets", Config: ConfigRequired}, Root: root}
			pages, err := CollectPages(source, testCase.config)
			if testCase.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
					t.Fatalf("CollectPages() error = %v, want %q", err, testCase.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := make(map[string]string, len(pages))
			for _, page := range pages {
				got[page.SourcePath] = page.Route
			}
			if !reflect.DeepEqual(got, testCase.want) {
				t.Fatalf("routes = %#v, want %#v", got, testCase.want)
			}
		})
	}
}

func TestCollectPagesSkipsPaths(t *testing.T) {
	root := t.TempDir()
	for _, file := range []string{"_partial.md", "_drafts/draft.md", "index.md", "snake_case.md"} {
		writeFixture(t, root, file, "# Page")
	}
	pages, err := CollectPages(FetchedSource{BundleSource: BundleSource{Repo: "acme/widgets", Config: ConfigDefault}, Root: root}, PublishConfig{Exclude: []string{}, Sections: []SectionRule{}})
	if err != nil {
		t.Fatal(err)
	}
	got := []string{pages[0].SourcePath, pages[1].SourcePath}
	if !reflect.DeepEqual(got, []string{"index.md", "snake_case.md"}) {
		t.Fatalf("page paths = %#v", got)
	}
}

func TestCollectPagesPrunesInaccessibleDirectories(t *testing.T) {
	root := t.TempDir()
	for _, file := range []string{
		"node_modules/x/page.md",
		".git/ignored.md",
		".github/PULL_REQUEST_TEMPLATE.md",
	} {
		writeFixture(t, root, file, "# Page")
	}

	directories := []string{filepath.Join(root, "node_modules"), filepath.Join(root, ".git")}
	for _, directory := range directories {
		if err := os.Chmod(directory, 0o000); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		for _, directory := range directories {
			if err := os.Chmod(directory, 0o700); err != nil {
				t.Errorf("restoring permissions for %s: %v", directory, err)
			}
		}
	}()

	for _, directory := range directories {
		if _, err := os.ReadDir(directory); err == nil {
			t.Fatalf("ReadDir(%q) succeeded despite mode 000", directory)
		}
	}

	pages, err := CollectPages(
		FetchedSource{BundleSource: BundleSource{Repo: "acme/widgets", Config: ConfigDefault}, Root: root},
		PublishConfig{Exclude: []string{}, Sections: []SectionRule{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 1 || pages[0].SourcePath != ".github/PULL_REQUEST_TEMPLATE.md" {
		t.Fatalf("collected pages = %#v, want only the PR template", pages)
	}
}
