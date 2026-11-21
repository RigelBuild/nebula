// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/RigelBuild/nebula/tools/go-lib/serviceurls"
)

const githubRepoHost = "github.com/RigelBuild"

type endpoint struct {
	host   string
	port   *int
	origin string
	owner  string
}

// coverageOwners holds the inventory-row source prefixes each generator owns.
type coverageOwners struct {
	kubernetes string
	dns        string
	cloudflare string
	workers    string
}

func coverageGaps(endpoints []endpoint, repos, cloudflare stringLists, inventory serviceurls.Inventory, owners coverageOwners) []string {
	gaps := make(map[string]struct{})
	if len(endpoints) == 0 {
		gaps["no endpoints found; refusing empty coverage"] = struct{}{}
	}
	addMissingEndpointRows(gaps, endpoints, inventory)
	addStaleOwnedRows(gaps, endpoints, inventory, owners)
	addWrongGateRows(gaps, cloudflare, inventory)
	addRepositoryGaps(gaps, repos, inventory)
	result := make([]string, 0, len(gaps))
	for gap := range gaps {
		result = append(result, gap)
	}
	slices.Sort(result)
	return result
}

func addMissingEndpointRows(gaps map[string]struct{}, endpoints []endpoint, inventory serviceurls.Inventory) {
	for _, source := range endpoints {
		if !slices.ContainsFunc(inventory.Links, func(link serviceurls.Link) bool {
			return endpointMatches(link, source)
		}) {
			port := "any"
			if source.port != nil {
				port = strconv.Itoa(*source.port)
			}
			gaps[fmt.Sprintf("missing row: %s:%s (%s)", source.host, port, source.origin)] = struct{}{}
		}
	}
}

func addStaleOwnedRows(gaps map[string]struct{}, endpoints []endpoint, inventory serviceurls.Inventory, owners coverageOwners) {
	for _, link := range inventory.Links {
		for _, owner := range []string{owners.kubernetes, owners.dns, owners.cloudflare, owners.workers} {
			matchesOwner := strings.HasPrefix(link.Source, owner)
			matchesEndpoint := slices.ContainsFunc(endpoints, func(source endpoint) bool {
				return endpointMatches(link, source)
			})
			if matchesOwner && !matchesEndpoint {
				gaps["stale row: "+link.URL] = struct{}{}
				break
			}
		}
	}
}

func addWrongGateRows(gaps map[string]struct{}, cloudflare stringLists, inventory serviceurls.Inventory) {
	access := make(map[string]struct{}, len(cloudflare.Access))
	for _, host := range cloudflare.Access {
		access[strings.ToLower(host)] = struct{}{}
	}
	for _, link := range inventory.Links {
		host, port, err := serviceurls.LinkHost(link)
		if err != nil {
			continue
		}
		_, isAccessHost := access[host]
		gated := isAccessHost && port == 443
		if gated != (link.Access == serviceurls.AccessCloudflareAccess) {
			gaps["wrong gate: "+link.URL] = struct{}{}
		}
	}
}

func addRepositoryGaps(gaps map[string]struct{}, repos stringLists, inventory serviceurls.Inventory) {
	managed := make(map[string]struct{}, len(repos.Active)+len(repos.Unmanaged))
	allRepos := append(slices.Clone(repos.Active), repos.Unmanaged...)
	for _, repo := range allRepos {
		managed[strings.ToLower(repo)] = struct{}{}
	}
	linked := make(map[string]struct{})
	for _, link := range inventory.Links {
		repo, found, err := serviceurls.LinkRepo(link)
		if err != nil || !found {
			continue
		}
		linked[repo] = struct{}{}
		if _, exists := managed[repo]; !exists {
			gaps["stale row: "+link.URL] = struct{}{}
		}
	}
	for repo := range managed {
		if _, exists := linked[repo]; !exists {
			gaps[fmt.Sprintf("missing row: %s/%s:443 (managed repository)", githubRepoHost, repo)] = struct{}{}
		}
	}
}

func endpointMatches(link serviceurls.Link, source endpoint) bool {
	host, port, err := serviceurls.LinkHost(link)
	if err != nil || host != source.host {
		return false
	}
	return source.port == nil || port == *source.port
}
