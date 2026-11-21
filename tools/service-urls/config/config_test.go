// SPDX-License-Identifier: MIT OR Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLoadAcceptsCompleteConfig(t *testing.T) {
	cfg, err := Load(filepath.Join("testdata", "valid.cue"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.InternalHostSuffixes, []string{"internal.example.com"}) {
		t.Fatalf("InternalHostSuffixes = %#v", cfg.InternalHostSuffixes)
	}
	if cfg.TailscaleDomain != "ts.example.net" || cfg.DNSZone != "internal.example.com" {
		t.Fatalf("domains = %q %q", cfg.TailscaleDomain, cfg.DNSZone)
	}
	if cfg.InventoryPath != "docs/service-urls" || cfg.PortalSource != "example/portal/" {
		t.Fatalf("paths = %q %q", cfg.InventoryPath, cfg.PortalSource)
	}
	want := Owners{Kubernetes: "example/k8s/", DNS: "example/dns/", Cloudflare: "example/cloudflare/", Workers: "example/workers/"}
	if cfg.Owners != want {
		t.Fatalf("Owners = %#v, want %#v", cfg.Owners, want)
	}
}

func TestLoadRejectsMissingOrEmptyValues(t *testing.T) {
	valid, err := os.ReadFile(filepath.Join("testdata", "valid.cue"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(valid)
	type edit struct {
		name string
		edit func(string) string
	}
	tests := []edit{
		{name: "empty internal host suffix list", edit: func(s string) string {
			return strings.Replace(s, `["internal.example.com"]`, `[]`, 1)
		}},
		{name: "empty internal host suffix", edit: func(s string) string {
			return strings.Replace(s, `["internal.example.com"]`, `[""]`, 1)
		}},
		{name: "empty later internal host suffix", edit: func(s string) string {
			return strings.Replace(s, `["internal.example.com"]`, `["a", ""]`, 1)
		}},
		{name: "missing owners", edit: func(s string) string {
			return s[:strings.Index(s, "owners:")]
		}},
		{name: "unknown field", edit: func(s string) string { return s + "\nextra: \"value\"\n" }},
		{name: "unknown owner", edit: func(s string) string {
			return strings.Replace(s, `workers:    "example/workers/"`, `workers:    "example/workers/"`+"\n\textra: \"example/extra/\"", 1)
		}},
	}
	for _, field := range []string{
		"internalHostSuffixes", "tailscaleDomain", "dnsZone", "inventoryPath", "linksPagePath", "glancePath",
		"portalSource", "tailnetPath", "unionPath", "dnsRecordsPath", "managedReposPath",
		"cloudflareHostsPath", "workerDomainsPath", "kubernetes", "dns", "cloudflare", "workers",
	} {
		tests = append(tests,
			edit{name: "missing " + field, edit: func(s string) string { return dropLine(s, field+":") }},
			edit{name: "empty " + field, edit: func(s string) string { return setEmpty(t, s, field) }},
		)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			edited := test.edit(text)
			if edited == text {
				t.Fatal("edit did not change the config")
			}
			path := filepath.Join(t.TempDir(), "config.cue")
			if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("Load() succeeded, want an error")
			}
		})
	}
}

// setEmpty replaces the value of the line that starts with field with "".
func setEmpty(t *testing.T, text, field string) string {
	t.Helper()
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, field+":") {
			lines[i] = line[:strings.Index(line, field)] + field + `: ""`
			return strings.Join(lines, "\n")
		}
	}
	t.Fatalf("field %s not found", field)
	return text
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.cue")); err == nil {
		t.Fatal("Load() succeeded for a missing file")
	}
}

// dropLine removes every line whose trimmed text starts with prefix.
func dropLine(text, prefix string) string {
	kept := make([]string, 0)
	for line := range strings.SplitSeq(text, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), prefix) {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}
