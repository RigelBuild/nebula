// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/RigelBuild/nebula/tools/go-lib/serviceurls"
)

func TestRenderPage(t *testing.T) {
	inventory := serviceurls.Inventory{
		Groups: []serviceurls.Group{
			{ID: "services", Title: "Services"},
			{ID: "tools", Title: "Tools"},
		},
		Links: []serviceurls.Link{
			{Group: "services", Name: "A [label]\\ path", URL: "https://example.com/path", What: "Example service", Access: serviceurls.AccessPublic, Kind: serviceurls.KindWeb, Via: "DNS", Login: "Entra OIDC", Node: "node-a", Source: "example/tailnet.toml"},
			{Group: "tools", Name: "API", URL: "tcp://example.com:9000", What: "Endpoint", Access: serviceurls.AccessTailnet, Kind: serviceurls.KindEndpoint, Via: "Fixture"},
		},
	}
	got, err := renderPage(inventory, exampleConfig())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(got, "## Services") > strings.Index(got, "## Tools") {
		t.Fatal("renderPage() did not preserve group order")
	}
	for _, want := range []string{
		"Generated from [`docs/service-urls/`](./service-urls/). Do not edit by hand.",
		"| Link | What | Gate | Port | Node | Via | Source |",
		"[A \\[label\\]\\\\ path](<https://example.com/path>)",
		"public + Entra OIDC | 443 | node-a",
		"`example/tailnet.toml`",
		"| tcp://example.com:9000 | Endpoint | tailnet | 9000 |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("renderPage() does not contain %q", want)
		}
	}
}

func TestRenderPageEscapesLabels(t *testing.T) {
	tests := []struct {
		name     string
		label    string
		wantLink string
	}{
		{name: "pipe", label: "left|right", wantLink: "[left\\|right](<https://example.com>)"},
		{name: "carriage return and line feed", label: "left\r\nright", wantLink: "[left right](<https://example.com>)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := renderPage(serviceurls.Inventory{
				Groups: []serviceurls.Group{{ID: "services", Title: "Services"}},
				Links:  []serviceurls.Link{{Group: "services", Name: test.label, URL: "https://example.com", What: "Example", Access: serviceurls.AccessPublic, Kind: serviceurls.KindWeb}},
			}, exampleConfig())
			if err != nil {
				t.Fatal(err)
			}
			wantRow := "| " + test.wantLink + " | Example | public | 443 |  |  |  |"
			if !strings.Contains(got, wantRow) {
				t.Fatalf("renderPage() missing row %q in:\n%s", wantRow, got)
			}
			rowCount := 0
			for _, row := range strings.Split(got, "\n") {
				if strings.Contains(row, test.wantLink) {
					rowCount++
					if row != wantRow {
						t.Errorf("rendered link occupies row %q, want %q", row, wantRow)
					}
				}
			}
			if rowCount != 1 {
				t.Errorf("rendered link appears on %d rows, want one", rowCount)
			}
		})
	}
}

func TestRenderGlance(t *testing.T) {
	cfg := exampleConfig()
	inventory := serviceurls.Inventory{
		Groups: []serviceurls.Group{
			{ID: "services", Title: "Services"},
			{ID: "tools", Title: "Tools"},
			{ID: "tcp", Title: "TCP only"},
		},
		Links: []serviceurls.Link{
			{Group: "services", Name: "Sample", URL: "https://example.com", What: "Example", Kind: serviceurls.KindWeb},
			{Group: "tools", Name: "Tool", URL: "https://tool.example.com", What: "Tool description", Kind: serviceurls.KindWeb},
			{Group: "tools", Name: "Alias", URL: "https://alias.example.com", What: "Alias", Kind: serviceurls.KindRedirect},
			{Group: "tools", Name: "Portal", URL: "https://portal.example.com", What: "Portal", Kind: serviceurls.KindWeb, Source: cfg.PortalSource},
			{Group: "tcp", Name: "Raw", URL: "tcp://raw.example.com:22", What: "TCP", Kind: serviceurls.KindEndpoint},
		},
	}
	got, err := renderGlance(inventory, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# Generated from docs/service-urls/. Do not edit by hand.",
		"name: Internal links",
		"title: Services",
		"title: Sample",
		"title: Tool",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("renderGlance() does not contain %q", want)
		}
	}
	for _, absent := range []string{"title: Alias", "title: Portal", "title: TCP only"} {
		if strings.Contains(got, absent) {
			t.Errorf("renderGlance() unexpectedly contains %q", absent)
		}
	}
}

func TestRenderPageHeaderLinkIsRelativeToPage(t *testing.T) {
	tests := []struct {
		name      string
		pagePath  string
		inventory string
		want      string
	}{
		{name: "sibling directory", pagePath: "docs/links.md", inventory: "docs/service-urls", want: "Generated from [`docs/service-urls/`](./service-urls/). Do not edit by hand."},
		{name: "page at root", pagePath: "links.md", inventory: "docs/service-urls", want: "Generated from [`docs/service-urls/`](./docs/service-urls/). Do not edit by hand."},
		{name: "page in other tree", pagePath: "site/pages/links.md", inventory: "docs/service-urls", want: "Generated from [`docs/service-urls/`](./../../docs/service-urls/). Do not edit by hand."},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := exampleConfig()
			cfg.LinksPagePath = test.pagePath
			cfg.InventoryPath = test.inventory
			got, err := renderPage(serviceurls.Inventory{}, cfg)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(got, "\n")
			if len(lines) < 3 || lines[2] != test.want {
				t.Fatalf("header line = %q, want %q", lines[2], test.want)
			}
		})
	}
}
