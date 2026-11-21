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
	for _, test := range []struct {
		name string
		edit func(string) string
	}{
		{name: "missing internal host suffixes", edit: func(s string) string { return dropLine(s, "internalHostSuffixes:") }},
		{name: "empty internal host suffix list", edit: func(s string) string {
			return strings.Replace(s, `["internal.example.com"]`, `[]`, 1)
		}},
		{name: "empty internal host suffix", edit: func(s string) string {
			return strings.Replace(s, `["internal.example.com"]`, `[""]`, 1)
		}},
		{name: "missing glance path", edit: func(s string) string { return dropLine(s, "glancePath:") }},
		{name: "empty owner", edit: func(s string) string {
			return strings.Replace(s, `kubernetes: "example/k8s/"`, `kubernetes: ""`, 1)
		}},
		{name: "missing owner", edit: func(s string) string { return dropLine(s, "workers:") }},
		{name: "unknown field", edit: func(s string) string { return s + "\nextra: \"value\"\n" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.cue")
			if err := os.WriteFile(path, []byte(test.edit(text)), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("Load() succeeded, want an error")
			}
		})
	}
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
