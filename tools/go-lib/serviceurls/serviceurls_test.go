package serviceurls

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var testOptions = Options{InternalHostSuffixes: []string{"internal.example.com", "tailnet.example.com"}}

const groupFixture = "[[group]]\nid = \"services\"\ntitle = \"Services\"\n"
const linkFixture = "[[link]]\ngroup = \"services\"\nname = \"Sample\"\nurl = \"https://example.com/path\"\nwhat = \"Example service\"\naccess = \"public\"\nkind = \"web\"\nvia = \"DNS\"\n"

func TestParseInventory(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		wantErr []string
	}{
		{name: "preserves group and link order", text: groupFixture + "\n" + linkFixture},
		{name: "rejects empty group fields and optional values", text: "[[group]]\nid = \"\"\ntitle = \"  \"\n\n" + strings.Replace(linkFixture, "via = \"DNS\"", "via = \"DNS\"\nlogin = \"\"", 1), wantErr: []string{"group[0].id is empty", "group[0].title is empty", "link[0].login is empty"}},
		{name: "requires an absolute URL", text: groupFixture + strings.Replace(linkFixture, "https://example.com/path", "/relative", 1), wantErr: []string{"link[0].url must be an absolute URL"}},
		{name: "rejects URL credentials", text: groupFixture + strings.Replace(linkFixture, "https://example.com/path", "https://user:pass@example.com/path", 1), wantErr: []string{"link[0].url must not carry credentials"}},
		{name: "rejects URL angle brackets", text: groupFixture + strings.Replace(linkFixture, "https://example.com/path", "https://example.com/<path>", 1), wantErr: []string{"link[0].url must not contain angle brackets, pipes, or whitespace"}},
		{name: "rejects URL whitespace", text: groupFixture + strings.Replace(linkFixture, "https://example.com/path", "https://example.com/a b", 1), wantErr: []string{"link[0].url must not contain angle brackets, pipes, or whitespace"}},
		{name: "rejects URL table pipes", text: groupFixture + strings.Replace(linkFixture, "https://example.com/path", "https://example.com/a|b", 1), wantErr: []string{"link[0].url must not contain angle brackets, pipes, or whitespace"}},
		{name: "rejects source backticks", text: groupFixture + strings.Replace(linkFixture, "via = \"DNS\"", "via = \"DNS\"\nsource = \"infra/nix/`service.nix\"", 1), wantErr: []string{"link[0].source must not contain backticks"}},
		{name: "lists empty required fields", text: groupFixture + strings.Replace(strings.Replace(linkFixture, "name = \"Sample\"", "name = \"\"", 1), "via = \"DNS\"", "via = \"\"", 1), wantErr: []string{"link[0].name is empty", "link[0].via is empty"}},
		{name: "rejects undeclared groups", text: groupFixture + strings.Replace(linkFixture, "group = \"services\"", "group = \"missing\"", 1), wantErr: []string{"link[0].group references undeclared group \"missing\""}},
		{name: "rejects duplicate raw URLs", text: groupFixture + linkFixture + "\n" + strings.Replace(linkFixture, "name = \"Sample\"", "name = \"Second\"", 1), wantErr: []string{"duplicate url \"https://example.com/path\""}},
		{name: "rejects duplicate canonical URLs", text: groupFixture + linkFixture + "\n" + strings.Replace(strings.Replace(linkFixture, "name = \"Sample\"", "name = \"Second\"", 1), "https://example.com/path", "https://EXAMPLE.com:443/path", 1), wantErr: []string{"duplicate url \"https://example.com/path\""}},
		{name: "rejects dot-segment URL aliases", text: groupFixture + linkFixture + "\n" + strings.Replace(strings.Replace(linkFixture, "name = \"Sample\"", "name = \"Second\"", 1), "https://example.com/path", "https://example.com/a/../path", 1), wantErr: []string{"duplicate url \"https://example.com/path\""}},
		{name: "rejects unknown group keys", text: strings.Replace(groupFixture, "title = \"Services\"", "title = \"Services\"\nextra = \"value\"", 1) + linkFixture, wantErr: []string{"group[0] has unknown key \"extra\""}},
		{name: "rejects unknown link keys", text: groupFixture + strings.Replace(linkFixture, "via = \"DNS\"", "via = \"DNS\"\nextra = \"value\"", 1), wantErr: []string{"link[0] has unknown key \"extra\""}},
		{name: "rejects unknown inventory keys", text: "title = \"stray\"\n" + groupFixture + linkFixture, wantErr: []string{"inventory has unknown key \"title\""}},
		{name: "requires HTTPS for web links", text: groupFixture + strings.Replace(linkFixture, "https://", "http://", 1), wantErr: []string{"link[0].url must use https for kind web"}},
		{name: "requires tailnet for internal hostname", text: groupFixture + strings.Replace(linkFixture, "https://example.com", "https://service.internal.example.com", 1), wantErr: []string{"link[0].access must be tailnet for internal host"}},
		{name: "requires tailnet for trailing-dot internal hostname", text: groupFixture + strings.Replace(linkFixture, "https://example.com", "https://service.internal.example.com.", 1), wantErr: []string{"link[0].access must be tailnet for internal host"}},
		{name: "requires tailnet for second suffix hostname", text: groupFixture + strings.Replace(linkFixture, "https://example.com", "https://node.tailnet.example.com", 1), wantErr: []string{"link[0].access must be tailnet for internal host"}},
		{name: "requires tailnet for trailing-dot second suffix hostname", text: groupFixture + strings.Replace(linkFixture, "https://example.com", "https://node.tailnet.example.com.", 1), wantErr: []string{"link[0].access must be tailnet for internal host"}},
		{name: "aggregates all violations", text: groupFixture + strings.Replace(strings.Replace(strings.Replace(strings.Replace(linkFixture, "group = \"services\"", "group = \"missing\"", 1), "name = \"Sample\"", "name = \"\"", 1), "https://example.com", "http://service.internal.example.com", 1), "via = \"DNS\"", "via = \"\"", 1), wantErr: []string{"link[0].group references undeclared group \"missing\"", "link[0].name is empty", "link[0].via is empty", "must use https", "must be tailnet"}},
		{name: "accepts redirects only over HTTPS", text: groupFixture + strings.Replace(linkFixture, "kind = \"web\"", "kind = \"redirect\"", 1)},
		{name: "allows HTTP endpoint URLs", text: groupFixture + strings.Replace(strings.Replace(linkFixture, "https://", "http://", 1), "kind = \"web\"", "kind = \"endpoint\"", 1)},
		{name: "rejects malformed TOML", text: "[[group]\nid = \"x\"", wantErr: []string{"service URL inventory is invalid TOML"}},
		{name: "suffix match needs a label boundary", text: groupFixture + strings.Replace(linkFixture, "https://example.com", "https://notinternal.example.com", 1)},
		{name: "rejects non-table top level", text: "\"value\"", wantErr: []string{"service URL inventory must be a TOML table"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inventory, err := ParseInventory(test.text, testOptions)
			if len(test.wantErr) > 0 {
				if err == nil {
					t.Fatal("ParseInventory() error = nil")
				}
				for _, part := range test.wantErr {
					if !strings.Contains(err.Error(), part) {
						t.Errorf("ParseInventory() error %q does not contain %q", err, part)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseInventory() error = %v", err)
			}
			if len(inventory.Groups) != 1 || len(inventory.Links) != 1 {
				t.Fatalf("ParseInventory() = %#v, want one group and one link", inventory)
			}
			if inventory.Links[0].Login != "" || inventory.Links[0].Source != "" {
				t.Errorf("ParseInventory() optional values = %#v", inventory.Links[0])
			}
		})
	}
}

func TestReadInventory(t *testing.T) {
	t.Run("merges group and link files and ignores other files", func(t *testing.T) {
		dir := t.TempDir()
		writeInventoryFile(t, dir, "groups.toml", groupFixture)
		writeInventoryFile(t, dir, "services.toml", linkFixture)
		writeInventoryFile(t, dir, "notes.md", "not inventory")
		inventory, err := ReadInventory(dir, testOptions)
		if err != nil {
			t.Fatalf("ReadInventory() error = %v", err)
		}
		if len(inventory.Groups) != 1 || len(inventory.Links) != 1 {
			t.Fatalf("ReadInventory() = %#v, want one group and one link", inventory)
		}
	})
	t.Run("names the file that is not valid TOML", func(t *testing.T) {
		dir := t.TempDir()
		writeInventoryFile(t, dir, "groups.toml", groupFixture)
		writeInventoryFile(t, dir, "services.toml", "[[link]")
		_, err := ReadInventory(dir, testOptions)
		if err == nil || !strings.Contains(err.Error(), "services.toml: ") {
			t.Fatalf("ReadInventory() error = %v, want the file name", err)
		}
	})
	t.Run("merges link files in name order", func(t *testing.T) {
		dir := t.TempDir()
		writeInventoryFile(t, dir, "groups.toml", groupFixture)
		first := strings.Replace(strings.Replace(linkFixture, "name = \"Sample\"", "name = \"First\"", 1), "https://example.com/path", "https://example.com/first", 1)
		second := strings.Replace(strings.Replace(linkFixture, "name = \"Sample\"", "name = \"Second\"", 1), "https://example.com/path", "https://example.com/second", 1)
		writeInventoryFile(t, dir, "z-links.toml", second)
		writeInventoryFile(t, dir, "a-links.toml", first)
		inventory, err := ReadInventory(dir, testOptions)
		if err != nil {
			t.Fatalf("ReadInventory() error = %v", err)
		}
		if len(inventory.Links) != 2 || inventory.Links[0].Name != "First" || inventory.Links[1].Name != "Second" {
			t.Fatalf("ReadInventory() links = %#v, want First then Second", inventory.Links)
		}
	})
	t.Run("names source file in row validation error", func(t *testing.T) {
		dir := t.TempDir()
		writeInventoryFile(t, dir, "groups.toml", groupFixture)
		firstLink := strings.Replace(linkFixture, "https://example.com/path", "https://example.com/first", 1)
		badLink := strings.Replace(linkFixture, "https://example.com/path", "http://example.com/bad", 1)
		writeInventoryFile(t, dir, "a-links.toml", firstLink)
		writeInventoryFile(t, dir, "z-bad-links.toml", badLink)
		_, err := ReadInventory(dir, testOptions)
		if err == nil || !strings.Contains(err.Error(), "z-bad-links.toml link[0].url must use https") {
			t.Fatalf("ReadInventory() error = %v, want file name and file-local row path", err)
		}
	})
	t.Run("names file and unknown top-level key", func(t *testing.T) {
		dir := t.TempDir()
		writeInventoryFile(t, dir, "groups.toml", groupFixture)
		writeInventoryFile(t, dir, "bad-links.toml", "title = \"stray\"\n")
		_, err := ReadInventory(dir, testOptions)
		if err == nil || !strings.Contains(err.Error(), `bad-links.toml: service URL inventory has unknown key "title"`) {
			t.Fatalf("ReadInventory() error = %v, want file name and unknown key", err)
		}
	})
	t.Run("requires groups file", func(t *testing.T) {
		dir := t.TempDir()
		_, err := ReadInventory(dir, testOptions)
		if err == nil || !strings.Contains(err.Error(), "missing groups.toml") {
			t.Fatalf("ReadInventory() error = %v, want missing groups.toml", err)
		}
	})
	t.Run("requires groups file to contain only groups", func(t *testing.T) {
		dir := t.TempDir()
		writeInventoryFile(t, dir, "groups.toml", groupFixture+linkFixture)
		_, err := ReadInventory(dir, testOptions)
		if err == nil || !strings.Contains(err.Error(), "groups.toml: service URL inventory file must contain only [[group]] tables, found [[link]]") {
			t.Fatalf("ReadInventory() error = %v, want groups.toml table-kind error", err)
		}
	})
	t.Run("requires link files to contain only links", func(t *testing.T) {
		dir := t.TempDir()
		writeInventoryFile(t, dir, "groups.toml", groupFixture)
		writeInventoryFile(t, dir, "services.toml", linkFixture+groupFixture)
		_, err := ReadInventory(dir, testOptions)
		if err == nil || !strings.Contains(err.Error(), "services.toml: service URL inventory file must contain only [[link]] tables, found [[group]]") {
			t.Fatalf("ReadInventory() error = %v, want link-file table-kind error", err)
		}
	})
	t.Run("names file and index for non-table row", func(t *testing.T) {
		dir := t.TempDir()
		writeInventoryFile(t, dir, "groups.toml", groupFixture)
		writeInventoryFile(t, dir, "bad-links.toml", "link = [1]\n")
		_, err := ReadInventory(dir, testOptions)
		if err == nil || !strings.Contains(err.Error(), "bad-links.toml: link[0] must be a table") {
			t.Fatalf("ReadInventory() error = %v, want file name and raw row index", err)
		}
	})
}

func writeInventoryFile(t *testing.T, dir, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Expected values are WHATWG `new URL(u).href` outputs.
func TestParseURLMatchesWHATWGDotSegments(t *testing.T) {
	for input, want := range map[string]string{
		"https://example.com/a/../path":      "https://example.com/path",
		"https://example.com/a/./b/":         "https://example.com/a/b/",
		"https://example.com/x/../y?q=1#f":   "https://example.com/y?q=1#f",
		"https://example.com/%2e%2e/x":       "https://example.com/x",
		"https://example.com/a/%2E/b":        "https://example.com/a/b",
		"https://example.com/a/..":           "https://example.com/",
		"https://example.com/a/b/.":          "https://example.com/a/b/",
		"https://example.com/../../x":        "https://example.com/x",
		"https://example.com/a%2Fb/c":        "https://example.com/a%2Fb/c",
		"https://example.com/sp%20ace/../x/": "https://example.com/x/",
	} {
		violations := []string{}
		parser := linkParser{violations: &violations, seenURLs: map[string]struct{}{}, internalSuffixes: testOptions.InternalHostSuffixes}
		got := parser.parseURL(input, "link[0]")
		if got == nil || got.String() != want || len(violations) != 0 {
			t.Errorf("parseURL(%q)=%v violations=%v, want %q", input, got, violations, want)
		}
	}
}

func TestLinkHost(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
		port int
	}{
		{name: "lowercase host and explicit port", url: "https://EXAMPLE.COM:9443/ui", want: "example.com", port: 9443},
		{name: "remove trailing host dot", url: "https://EXAMPLE.COM.:9443/ui", want: "example.com", port: 9443},
		{name: "HTTP explicit default port", url: "http://example.com:80", want: "example.com", port: 80},
		{name: "HTTP default port", url: "http://example.com", want: "example.com", port: 80},
		{name: "HTTPS default port", url: "https://example.com", want: "example.com", port: 443},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			host, port, err := LinkHost(Link{URL: test.url})
			if err != nil {
				t.Fatal(err)
			}
			if host != test.want || port != test.port {
				t.Fatalf("LinkHost() = (%q, %d), want (%q, %d)", host, port, test.want, test.port)
			}
		})
	}
}

func TestLinkRepo(t *testing.T) {
	tests := []struct {
		name  string
		url   string
		want  string
		found bool
	}{
		{name: "RigelBuild repo is lowercased", url: "https://github.com/RigelBuild/Krypton", want: "krypton", found: true},
		{name: "other owner is not a RigelBuild repo", url: "https://github.com/other/compass"},
		{name: "HTTP is not accepted", url: "http://github.com/RigelBuild/krypton"},
		{name: "nested repo is not accepted", url: "https://github.com/RigelBuild/krypton/docs"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, found, err := LinkRepo(Link{URL: test.url})
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want || found != test.found {
				t.Fatalf("LinkRepo() = (%q, %t), want (%q, %t)", got, found, test.want, test.found)
			}
		})
	}
}

func TestDanglingSources(t *testing.T) {
	inventory := Inventory{Links: []Link{{Source: "infra/k8s/platform"}, {Source: "infra/nix/tailnet.toml"}}}
	for _, test := range []struct {
		name    string
		tracked []string
		want    []string
	}{
		{name: "tracked file and source directory prefixes", tracked: []string{"infra/k8s/platform/ui.yaml", "infra/nix/tailnet.toml"}},
		{name: "reports both missing sources", tracked: []string{"infra/nix/flake.nix"}, want: []string{"infra/k8s/platform", "infra/nix/tailnet.toml"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := DanglingSources(inventory, test.tracked)
			if strings.Join(got, "\n") != strings.Join(test.want, "\n") {
				t.Fatalf("DanglingSources() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestTypeScriptPublicHelperParity(t *testing.T) {
	t.Run("tracked file and source directory prefixes", func(t *testing.T) {
		inventory := Inventory{Links: []Link{{Source: "infra/k8s/platform"}, {Source: "infra/nix/tailnet.toml"}}}
		if got := DanglingSources(inventory, []string{"infra/k8s/platform/ui.yaml", "infra/nix/tailnet.toml"}); len(got) != 0 {
			t.Fatalf("DanglingSources() = %#v", got)
		}
	})
	t.Run("HTTP default and explicit ports", func(t *testing.T) {
		for _, url := range []string{"http://example.com:80", "http://example.com"} {
			_, port, err := LinkHost(Link{URL: url})
			if err != nil || port != 80 {
				t.Fatalf("LinkHost(%q) = %d, %v", url, port, err)
			}
		}
	})
	t.Run("lowercase RigelBuild repository", func(t *testing.T) {
		got, ok, err := LinkRepo(Link{URL: "https://github.com/RigelBuild/Krypton"})
		if err != nil || !ok || got != "krypton" {
			t.Fatalf("LinkRepo() = %q, %t, %v", got, ok, err)
		}
	})
}

func TestInventoryRequiresInternalHostSuffix(t *testing.T) {
	text := groupFixture + linkFixture
	for _, opts := range []Options{{}, {InternalHostSuffixes: []string{" "}}} {
		if _, err := ParseInventory(text, opts); err == nil || !strings.Contains(err.Error(), "internal host suffix") {
			t.Fatalf("ParseInventory(%#v) error = %v, want a missing-suffix error", opts, err)
		}
		if _, err := ReadInventory(t.TempDir(), opts); err == nil || !strings.Contains(err.Error(), "internal host suffix") {
			t.Fatalf("ReadInventory(%#v) error = %v, want a missing-suffix error", opts, err)
		}
	}
}
