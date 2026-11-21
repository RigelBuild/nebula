// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"github.com/RigelBuild/nebula/tools/service-urls/config"
)

// exampleConfig is the deployment config the tests run against. Every value is
// a made-up example, so the fixtures carry no real host, path or owner.
// TailscaleDomain is the generic domain that full tailnet hosts end in.
func exampleConfig() config.Config {
	return config.Config{
		InternalHostSuffixes: []string{"internal.example.com"},
		TailscaleDomain:      "ts.example.net",
		DNSZone:              "internal.example.com",
		InventoryPath:        "docs/service-urls",
		LinksPagePath:        "docs/links.md",
		GlancePath:           "example/glance-config.yaml",
		PortalSource:         "example/portal/",
		TailnetPath:          "example/tailnet.toml",
		UnionPath:            "example/k8s/union.yaml",
		DNSRecordsPath:       "example/dns/records.json",
		ManagedReposPath:     "example/github/managed-repos.json",
		CloudflareHostsPath:  "example/cloudflare/hosts.json",
		WorkerDomainsPath:    "example/workers/sites.ts",
		Owners: config.Owners{
			Kubernetes: "example/k8s/",
			DNS:        "example/dns/",
			Cloudflare: "example/cloudflare/",
			Workers:    "example/workers/",
		},
	}
}
