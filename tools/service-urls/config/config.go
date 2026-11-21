// SPDX-License-Identifier: MIT OR Apache-2.0

// Package config loads the deployment values for the service links tool. The
// values are CUE, validated against the embedded schema. Every field is
// required, and the schema gives none a default.
package config

import (
	_ "embed"
	"fmt"
	"os"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"
)

//go:embed schema.cue
var schemaSource string

// Owners names the inventory-row source prefixes that each generator owns.
type Owners struct {
	Kubernetes string `json:"kubernetes"`
	DNS        string `json:"dns"`
	Cloudflare string `json:"cloudflare"`
	Workers    string `json:"workers"`
}

// Config is the validated deployment configuration. Paths are relative to the
// workspace root.
type Config struct {
	InternalHostSuffixes []string `json:"internalHostSuffixes"`
	TailscaleDomain      string   `json:"tailscaleDomain"`
	DNSZone              string   `json:"dnsZone"`
	InventoryPath        string   `json:"inventoryPath"`
	LinksPagePath        string   `json:"linksPagePath"`
	GlancePath           string   `json:"glancePath"`
	PortalSource         string   `json:"portalSource"`
	TailnetPath          string   `json:"tailnetPath"`
	UnionPath            string   `json:"unionPath"`
	DNSRecordsPath       string   `json:"dnsRecordsPath"`
	ManagedReposPath     string   `json:"managedReposPath"`
	CloudflareHostsPath  string   `json:"cloudflareHostsPath"`
	WorkerDomainsPath    string   `json:"workerDomainsPath"`
	Owners               Owners   `json:"owners"`
}

// Load reads the CUE config at path and validates it against the schema.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // The config path is an operator-supplied flag.
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	return decode(data, path)
}

func decode(data []byte, name string) (Config, error) {
	ctx := cuecontext.New()
	schema := ctx.CompileString(schemaSource, cue.Filename("schema.cue"))
	if err := schema.Err(); err != nil {
		return Config{}, fmt.Errorf("compile embedded schema: %w", err)
	}
	value := ctx.CompileBytes(data, cue.Filename(name))
	if err := value.Err(); err != nil {
		return Config{}, fmt.Errorf("evaluate %s: %w", name, err)
	}
	value = value.Unify(schema.LookupPath(cue.MakePath(cue.Def("Config"))))
	if err := value.Validate(cue.Concrete(true)); err != nil {
		return Config{}, fmt.Errorf("validate %s: %w", name, err)
	}
	var cfg Config
	if err := value.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode %s: %w", name, err)
	}
	return cfg, nil
}
