// SPDX-License-Identifier: MIT OR Apache-2.0

package bundle

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRouteSlug(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string
	}{
		{name: "index and mdx", path: "Guides/Index.mdx", want: "/guides"},
		{name: "readme", path: "README.md", want: "/readme"},
		{name: "punctuation", path: "Guides/Hello, World!.md", want: "/guides/hello-world"},
		{name: "unicode and emoji", path: "Café/猫 😀.md", want: "/café/猫-"},
		{name: "repeated spaces", path: "many   spaces.md", want: "/many---spaces"},
		{name: "underscore", path: "snake_case.md", want: "/snake_case"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := RouteSlug(testCase.path); got != testCase.want {
				t.Errorf("RouteSlug(%q) = %q, want %q", testCase.path, got, testCase.want)
			}
		})
	}
}

func TestGatherable(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"README.md", true}, {"docs/start.MD", true}, {".github/PULL_REQUEST_TEMPLATE.md", true},
		{"docs/snake_case.md", true}, {"docs/my_dir/page.md", true}, {"team/docs/designs/page.md", true},
		{".github/other.md", false}, {"CHANGELOG.md", false}, {"docs/designs/record.md", false},
		{"node_modules/x.md", false}, {"dist/x.md", false}, {"outputs/x.md", false},
		{".hidden/page.md", false}, {"_partial.md", false}, {"docs/_drafts/x.md", false},
		{"pkg/__tests__/x.md", false}, {"src/code.ts", false},
	}
	for _, testCase := range cases {
		if got := IsGatherable(testCase.path); got != testCase.want {
			t.Errorf("IsGatherable(%q) = %v, want %v", testCase.path, got, testCase.want)
		}
	}
}

func TestConfigAndPageCollection(t *testing.T) {
	root := t.TempDir()
	if _, err := ReadPublishConfig(root, ConfigRequired); err == nil || !strings.Contains(err.Error(), "docs-bundle.toml") {
		t.Fatalf("required missing config error = %v", err)
	}
	config, err := ReadPublishConfig(root, ConfigDefault)
	if err != nil || !reflect.DeepEqual(config, PublishConfig{Exclude: []string{}, Sections: []SectionRule{}}) {
		t.Fatalf("default config = %#v, %v", config, err)
	}
	writeFixture(t, root, "docs-bundle.toml", "exclude = [\"private/**\"]\n\n[[sections]]\nid = \"guide\"\nlabel = \"Guide\"\nroute = \"/guide\"\nprefix = \"docs/guide/\"\npagefind = false\n\n[packages]\nroute = \"/packages\"\nallow = [\"widgets/\"]\nreadme_depth = 3\nnested = [\"apps\"]\n")
	config, err = ReadPublishConfig(root, ConfigRequired)
	if err != nil {
		t.Fatal(err)
	}
	if config.Sections[0].Pagefind == nil || *config.Sections[0].Pagefind || config.Packages == nil || config.Packages.ReadmeDepth != 3 {
		t.Fatalf("parsed TOML config = %#v", config)
	}
	writeFixture(t, root, "packages/widgets/README.md", "# Widgets\n")
	writeFixture(t, root, "docs/guide/intro.md", "# Intro\n")
	writeFixture(t, root, "docs/_drafts/skip.md", "# Draft\n")
	writeFixture(t, root, "private/secret.md", "# Secret\n")
	pages, err := CollectPages(FetchedSource{BundleSource: BundleSource{Repo: "acme/widgets", Config: ConfigRequired}, Root: root}, config)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 || pages[0].Route != "/guide/intro" || pages[1].Route != "/packages/packages/widgets/readme" {
		t.Fatalf("collected pages = %#v", pages)
	}
	if err := AssertNoRouteCollisions([]BundlePage{{Route: "/guide"}, {Route: "/guide"}}); err == nil || !strings.Contains(err.Error(), "/guide") {
		t.Fatalf("route collision error = %v", err)
	}
}

func TestLinks(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "docs/index.md", "[exists](asset.png) [missing](missing.md)\n[ref]: absent.md\n```md\n[hidden]: skip.md\n```\n")
	writeFixture(t, root, "docs/asset.png", "image")
	current := BundlePage{Repo: "acme/widgets", SourcePath: "docs/index.md", Route: "/guide"}
	pages := []BundlePage{current}
	broken, err := FindBrokenLinks(root, current.Repo, pages)
	if err != nil {
		t.Fatal(err)
	}
	want := []BrokenLink{{Repo: current.Repo, SourcePath: current.SourcePath, Target: "docs/missing.md"}, {Repo: current.Repo, SourcePath: current.SourcePath, Target: "docs/absent.md"}}
	if !reflect.DeepEqual(broken, want) {
		t.Fatalf("FindBrokenLinks() = %#v, want %#v", broken, want)
	}
}

func writeFixture(t *testing.T, root, relative, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
