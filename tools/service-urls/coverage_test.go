// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/RigelBuild/nebula/tools/go-lib/serviceurls"
)

func testOwners() coverageOwners {
	cfg := exampleConfig()
	return coverageOwnersFrom(cfg)
}

func TestK8sEndpoints(t *testing.T) {
	cfg := exampleConfig()
	network := tailnet{
		MagicDNSSuffix: "tail-example.ts.example.net",
		Nodes: map[string]struct {
			FQDN string `toml:"fqdn"`
		}{
			"node": {FQDN: "node.example.com"},
		},
	}
	t.Run("finds every tailscale Service port and prefers its external DNS hostname", func(t *testing.T) {
		got, err := k8sEndpoints([]any{
			map[string]any{
				"kind": "Service",
				"metadata": map[string]any{
					"name": "public-name",
					"annotations": map[string]any{
						"external-dns.alpha.kubernetes.io/hostname": "svc.internal.example.com",
						"tailscale.com/hostname":                    "ignored",
					},
				},
				"spec": map[string]any{
					"loadBalancerClass": "tailscale",
					"ports":             []any{map[string]any{"port": 443}, map[string]any{"port": 8443}},
				},
			},
			map[string]any{
				"kind":     "Service",
				"metadata": map[string]any{"name": "fallback", "annotations": map[string]any{"tailscale.com/hostname": "node"}},
				"spec":     map[string]any{"loadBalancerClass": "tailscale", "ports": []any{map[string]any{"port": 9000}}},
			},
		}, network, cfg)
		if err != nil {
			t.Fatal(err)
		}
		want := []endpoint{
			{host: "svc.internal.example.com", port: new(443), origin: "public-name Service port 443", owner: cfg.Owners.Kubernetes},
			{host: "svc.internal.example.com", port: new(8443), origin: "public-name Service port 8443", owner: cfg.Owners.Kubernetes},
			{host: "node.example.com", port: new(9000), origin: "fallback Service port 9000", owner: cfg.Owners.Kubernetes},
		}
		assertEndpoints(t, got, want)
	})
	t.Run("finds each tailscale Ingress TLS host at HTTPS port", func(t *testing.T) {
		got, err := k8sEndpoints([]any{map[string]any{
			"kind":     "Ingress",
			"metadata": map[string]any{"name": "web", "namespace": "apps"},
			"spec": map[string]any{
				"ingressClassName": "tailscale",
				"tls":              []any{map[string]any{"hosts": []any{"node", "web.internal.example.com"}}},
			},
		}}, network, cfg)
		if err != nil {
			t.Fatal(err)
		}
		want := []endpoint{
			{host: "node.example.com", port: new(443), origin: "apps/web Ingress TLS host", owner: cfg.Owners.Kubernetes},
			{host: "web.internal.example.com", port: new(443), origin: "apps/web Ingress TLS host", owner: cfg.Owners.Kubernetes},
		}
		assertEndpoints(t, got, want)
	})
	t.Run("accepts a full Ingress TLS host with the network MagicDNS suffix", func(t *testing.T) {
		got, err := k8sEndpoints([]any{map[string]any{
			"kind":     "Ingress",
			"metadata": map[string]any{"name": "auth", "namespace": "authentik"},
			"spec": map[string]any{
				"ingressClassName": "tailscale",
				"tls":              []any{map[string]any{"hosts": []any{"auth.tail-example.ts.example.net"}}},
			},
		}}, network, cfg)
		if err != nil {
			t.Fatal(err)
		}
		want := []endpoint{
			{host: "auth.tail-example.ts.example.net", port: new(443), origin: "authentik/auth Ingress TLS host", owner: cfg.Owners.Kubernetes},
		}
		assertEndpoints(t, got, want)
	})
	t.Run("allows a full tailnet Ingress host with a proxy group", func(t *testing.T) {
		got, err := k8sEndpoints([]any{map[string]any{
			"kind": "Ingress",
			"metadata": map[string]any{
				"name":        "auth",
				"namespace":   "authentik",
				"annotations": map[string]any{"tailscale.com/proxy-group": "authentik-ingress"},
			},
			"spec": map[string]any{
				"ingressClassName": "tailscale",
				"tls":              []any{map[string]any{"hosts": []any{"auth.tail-example.ts.example.net"}}},
			},
		}}, network, cfg)
		if err != nil {
			t.Fatal(err)
		}
		want := []endpoint{
			{host: "auth.tail-example.ts.example.net", port: new(443), origin: "authentik/auth Ingress TLS host", owner: cfg.Owners.Kubernetes},
		}
		assertEndpoints(t, got, want)
	})
	t.Run("rejects a full tailscale-domain Ingress host with a mismatched suffix", func(t *testing.T) {
		_, err := k8sEndpoints([]any{map[string]any{
			"kind":     "Ingress",
			"metadata": map[string]any{"name": "auth", "namespace": "authentik"},
			"spec": map[string]any{
				"ingressClassName": "tailscale",
				"tls":              []any{map[string]any{"hosts": []any{"auth.other.ts.example.net"}}},
			},
		}}, network, cfg)
		if err == nil || !strings.Contains(err.Error(), `network MagicDNS suffix "tail-example.ts.example.net"`) {
			t.Fatalf("k8sEndpoints() error = %v, want mismatched MagicDNS suffix error", err)
		}
	})
	t.Run("rejects List resources without an items array", func(t *testing.T) {
		_, err := k8sEndpoints([]any{map[string]any{"kind": "List"}}, network, cfg)
		if err == nil || err.Error() != "kubernetes List has no items array" {
			t.Fatalf("k8sEndpoints() error = %v, want kubernetes List has no items array", err)
		}
	})
}

func TestDNSEndpointsReadsRecordInventory(t *testing.T) {
	cfg := exampleConfig()
	records := []dnsRecord{
		{Name: "svc-one", Host: "node-a"},
		{Name: "svc-two", Host: "node-b"},
		{Name: "svc-three", Host: "node-c"},
		{Name: "svc-four", Host: "node-d"},
	}
	got := dnsEndpoints(records, cfg)
	want := []endpoint{
		{host: "svc-one.internal.example.com", origin: "relativeRecordSetName for svc-one.internal.example.com", owner: cfg.Owners.DNS},
		{host: "svc-two.internal.example.com", origin: "relativeRecordSetName for svc-two.internal.example.com", owner: cfg.Owners.DNS},
		{host: "svc-three.internal.example.com", origin: "relativeRecordSetName for svc-three.internal.example.com", owner: cfg.Owners.DNS},
		{host: "svc-four.internal.example.com", origin: "relativeRecordSetName for svc-four.internal.example.com", owner: cfg.Owners.DNS},
	}
	assertEndpoints(t, got, want)
}

func TestCloudflareEndpoints(t *testing.T) {
	cfg := exampleConfig()
	got := cloudflareEndpoints(stringLists{Access: []string{"access.example.com"}, Proxied: []string{"web.example.com", "access.example.com"}}, cfg.Owners.Cloudflare)
	want := []endpoint{
		{host: "access.example.com", port: new(443), origin: "Cloudflare Access app access.example.com", owner: cfg.Owners.Cloudflare},
		{host: "web.example.com", port: new(443), origin: "proxied DNS web.example.com", owner: cfg.Owners.Cloudflare},
	}
	assertEndpoints(t, got, want)
}

func TestWorkerDomainEndpoints(t *testing.T) {
	cfg := exampleConfig()
	got, err := workerDomainEndpoints(`
		new cloudflare.WorkersCustomDomain("site-apex", { hostname: "example.com" });
		new cloudflare.WorkersCustomDomain("site-www", { hostname: "www.example.com" });
	`, cfg.Owners.Workers)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].host != "example.com" || got[1].host != "www.example.com" || got[0].owner != cfg.Owners.Workers || got[1].owner != cfg.Owners.Workers {
		t.Fatalf("workerDomainEndpoints() = %#v", got)
	}
}

func TestWorkerDomainEndpointsRequiresDomains(t *testing.T) {
	if _, err := workerDomainEndpoints(`const unrelated = 1;`, "example/workers/"); err == nil {
		t.Fatal("workerDomainEndpoints() succeeded with no Worker domains")
	}
}

func TestFleetFQDN(t *testing.T) {
	network := tailnet{MagicDNSSuffix: "tail-example.ts.example.net", Nodes: map[string]struct {
		FQDN string `toml:"fqdn"`
	}{"node": {FQDN: "node.example.com"}}}
	if got := fleetFQDN("node", network); got != "node.example.com" {
		t.Errorf("fleetFQDN(node) = %q", got)
	}
	if got := fleetFQDN("missing", network); got != "missing.tail-example.ts.example.net" {
		t.Errorf("fleetFQDN(missing) = %q", got)
	}
}

func TestHasInternalSuffix(t *testing.T) {
	suffixes := []string{"internal.example.com"}
	for _, test := range []struct {
		host string
		want bool
	}{
		{host: "svc.internal.example.com", want: true},
		{host: "deep.svc.internal.example.com", want: true},
		{host: "notinternal.example.com", want: false},
		{host: "example.com", want: false},
	} {
		if got := hasInternalSuffix(test.host, suffixes); got != test.want {
			t.Errorf("hasInternalSuffix(%q) = %v, want %v", test.host, got, test.want)
		}
	}
}

func TestCoverageGaps(t *testing.T) {
	inventory := serviceurls.Inventory{
		Groups: []serviceurls.Group{{ID: "services", Title: "Services"}},
		Links:  []serviceurls.Link{{Group: "services", Name: "Service", URL: "https://service.internal.example.com:8443", What: "Example", Access: serviceurls.AccessTailnet, Kind: serviceurls.KindEndpoint, Via: "Fixture", Source: "example/nix/example.nix"}},
	}
	endpointAnyPort := endpoint{host: "service.internal.example.com", origin: "DNS record", owner: exampleConfig().Owners.DNS}
	if got := coverageGaps([]endpoint{endpointAnyPort}, stringLists{}, stringLists{}, inventory, testOwners()); len(got) != 0 {
		t.Fatalf("coverageGaps() = %#v, want no gaps", got)
	}
	wrongPort := endpoint{host: "service.internal.example.com", port: new(9000), origin: "service Service port 9000", owner: exampleConfig().Owners.Kubernetes}
	wrongInventory := serviceurls.Inventory{Links: []serviceurls.Link{{URL: "tcp://service.internal.example.com:9001", Source: "example/k8s/service.yaml"}}}
	got := coverageGaps([]endpoint{wrongPort}, stringLists{}, stringLists{}, wrongInventory, testOwners())
	if len(got) != 2 || !strings.HasPrefix(got[0], "missing row:") || !strings.HasPrefix(got[1], "stale row:") {
		t.Fatalf("coverageGaps() = %#v, want one missing and one stale row", got)
	}
}

func TestCoverageGapsAccessAndRepos(t *testing.T) {
	cloudflare := stringLists{Access: []string{"secure.example.com"}}
	inventory := serviceurls.Inventory{Links: []serviceurls.Link{
		{URL: "https://secure.example.com", Access: serviceurls.AccessTailnet, Kind: serviceurls.KindWeb},
		{URL: "https://github.com/RigelBuild/unknown", Access: serviceurls.AccessPublic, Kind: serviceurls.KindWeb},
	}}
	gaps := coverageGaps(nil, stringLists{Active: []string{"known"}}, cloudflare, inventory, testOwners())
	for _, want := range []string{"no endpoints found; refusing empty coverage", "wrong gate: https://secure.example.com", "stale row: https://github.com/RigelBuild/unknown", "missing row: github.com/RigelBuild/known:443 (managed repository)"} {
		if !slices.Contains(gaps, want) {
			t.Errorf("coverageGaps() = %#v, missing %q", gaps, want)
		}
	}
}

func TestWorkerDomainsOwnInventoryRows(t *testing.T) {
	cfg := exampleConfig()
	endpoints, err := workerDomainEndpoints(`
		new cloudflare.WorkersCustomDomain("site-apex", { hostname: "example.com" });
		new cloudflare.WorkersCustomDomain("site-www", { hostname: "www.example.com" });
	`, cfg.Owners.Workers)
	if err != nil {
		t.Fatal(err)
	}
	rows := serviceurls.Inventory{Links: []serviceurls.Link{
		{URL: "https://example.com", Source: cfg.Owners.Workers},
		{URL: "https://www.example.com", Source: cfg.Owners.Workers},
	}}
	if got := coverageGaps(endpoints, stringLists{}, stringLists{}, rows, testOwners()); len(got) != 0 {
		t.Fatalf("coverageGaps() = %#v, want no gaps", got)
	}
	wwwOnly := serviceurls.Inventory{Links: []serviceurls.Link{{URL: "https://www.example.com", Source: cfg.Owners.Workers}}}
	gaps := coverageGaps(endpoints, stringLists{}, stringLists{}, wwwOnly, testOwners())
	if len(gaps) != 1 || !strings.Contains(gaps[0], "Worker custom domain example.com") {
		t.Fatalf("coverageGaps() = %#v, want missing example.com", gaps)
	}
}

func TestCoverageGapsAccessGateTransitions(t *testing.T) {
	for _, test := range []struct {
		name       string
		cloudflare stringLists
		access     serviceurls.Access
	}{
		{name: "Access host requires the Cloudflare Access gate", cloudflare: stringLists{Access: []string{"secure.example.com"}}, access: serviceurls.AccessTailnet},
		{name: "proxied host no longer allows the Access gate", cloudflare: stringLists{Proxied: []string{"secure.example.com"}}, access: serviceurls.AccessCloudflareAccess},
	} {
		t.Run(test.name, func(t *testing.T) {
			inventory := serviceurls.Inventory{Links: []serviceurls.Link{{URL: "https://secure.example.com", Access: test.access, Kind: serviceurls.KindWeb}}}
			got := coverageGaps(cloudflareEndpoints(test.cloudflare, exampleConfig().Owners.Cloudflare), stringLists{}, test.cloudflare, inventory, testOwners())
			want := []string{"wrong gate: https://secure.example.com"}
			if !slices.Equal(got, want) {
				t.Fatalf("coverageGaps() = %#v, want %#v", got, want)
			}
		})
	}
}

func TestCoverageGapsStaleRepositoryRow(t *testing.T) {
	inventory := serviceurls.Inventory{Links: []serviceurls.Link{{URL: "https://github.com/RigelBuild/unknown", Source: "example/github/index.ts"}}}
	gaps := coverageGaps([]endpoint{}, stringLists{Active: []string{"known"}}, stringLists{}, inventory, testOwners())
	want := []string{
		"missing row: github.com/RigelBuild/known:443 (managed repository)",
		"no endpoints found; refusing empty coverage",
		"stale row: https://github.com/RigelBuild/unknown",
	}
	if !slices.Equal(gaps, want) {
		t.Fatalf("coverageGaps() = %#v, want %#v", gaps, want)
	}
}

func TestReadCoverageInputsRejectsMissingRequiredFile(t *testing.T) {
	root := t.TempDir()
	if _, _, _, err := readCoverageInputs(root, exampleConfig()); err == nil {
		t.Fatal("readCoverageInputs() succeeded with no input files")
	}
}

func TestReadJSONDecodesRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.json")
	if err := os.WriteFile(path, []byte(`[{"name":"svc-one","host":"node-a"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	var records []dnsRecord
	if err := readJSON(path, &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Name != "svc-one" {
		t.Fatalf("readJSON() = %#v", records)
	}
	if err := json.Unmarshal([]byte(`not json`), &records); err == nil {
		t.Fatal("json.Unmarshal() accepted invalid JSON")
	}
}

func assertEndpoints(t *testing.T, got, want []endpoint) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("endpoints length = %d, want %d; got %#v", len(got), len(want), got)
	}
	for index := range want {
		portMatches := got[index].port == nil && want[index].port == nil
		if got[index].port != nil && want[index].port != nil {
			portMatches = *got[index].port == *want[index].port
		}
		if got[index].host != want[index].host || got[index].origin != want[index].origin || got[index].owner != want[index].owner || !portMatches {
			t.Errorf("endpoint[%d] = %#v, want %#v", index, got[index], want[index])
		}
	}
}
