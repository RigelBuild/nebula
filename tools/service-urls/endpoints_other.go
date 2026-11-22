// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"errors"
	"regexp"
	"strings"

	"github.com/RigelBuild/nebula/tools/service-urls/config"
)

type dnsRecord struct {
	Name string `json:"name"`
	Host string `json:"host"`
}

type stringLists struct {
	Active    []string `json:"active"`
	Unmanaged []string `json:"unmanaged"`
	Access    []string `json:"access"`
	Proxied   []string `json:"proxied"`
}

func dnsEndpoints(records []dnsRecord, cfg config.Config) []endpoint {
	endpoints := make([]endpoint, 0, len(records))
	seen := make(map[string]struct{}, len(records))
	for _, record := range records {
		host := strings.ToLower(record.Name + "." + cfg.DNSZone)
		if record.Name == "" {
			continue
		}
		if _, exists := seen[host]; exists {
			continue
		}
		seen[host] = struct{}{}
		endpoints = append(endpoints, endpoint{
			host:   host,
			origin: "relativeRecordSetName for " + host,
			owner:  cfg.Owners.DNS,
		})
	}
	return endpoints
}

func cloudflareEndpoints(hosts stringLists, owner string) []endpoint {
	access := make(map[string]struct{}, len(hosts.Access))
	all := make([]string, 0, len(hosts.Access)+len(hosts.Proxied))
	seen := make(map[string]struct{}, len(hosts.Access)+len(hosts.Proxied))
	for _, host := range hosts.Access {
		lower := strings.ToLower(host)
		access[lower] = struct{}{}
		if _, exists := seen[lower]; !exists {
			all = append(all, lower)
			seen[lower] = struct{}{}
		}
	}
	for _, host := range hosts.Proxied {
		lower := strings.ToLower(host)
		if _, exists := seen[lower]; !exists {
			all = append(all, lower)
			seen[lower] = struct{}{}
		}
	}
	endpoints := make([]endpoint, 0, len(all))
	for _, host := range all {
		port := 443
		origin := "proxied DNS " + host
		if _, exists := access[host]; exists {
			origin = "Cloudflare Access app " + host
		}
		endpoints = append(endpoints, endpoint{
			host:   host,
			port:   &port,
			origin: origin,
			owner:  owner,
		})
	}
	return endpoints
}

var workerDomainPattern = regexp.MustCompile(`new\s+cloudflare\.WorkersCustomDomain\(\s*"[^"]+"\s*,\s*\{[\s\S]*?\bhostname:\s*"([^"]+)"`)

func workerDomainEndpoints(source string, owner string) ([]endpoint, error) {
	matches := workerDomainPattern.FindAllStringSubmatch(source, -1)
	if len(matches) == 0 {
		return nil, errors.New("Worker stack declares no Worker custom domains")
	}
	endpoints := make([]endpoint, 0, len(matches))
	for _, match := range matches {
		host := strings.ToLower(match[1])
		port := 443
		endpoints = append(endpoints, endpoint{
			host:   host,
			port:   &port,
			origin: "Worker custom domain " + match[1],
			owner:  owner,
		})
	}
	return endpoints, nil
}
