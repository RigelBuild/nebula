// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/RigelBuild/nebula/tools/service-urls/config"
	"github.com/pelletier/go-toml/v2"
	"go.yaml.in/yaml/v4"
)

func runCoverage(root string, cfg config.Config, stdout, stderr io.Writer) int {
	inventory, err := readInventory(root, cfg)
	if err != nil {
		return writeFailure(stderr, "service-urls coverage: cannot read inventory: "+err.Error())
	}
	endpoints, repos, cloudflare, err := readCoverageInputs(root, cfg)
	if err != nil {
		return writeFailure(stderr, "service-urls coverage: cannot read inputs: "+err.Error())
	}
	gaps := coverageGaps(endpoints, repos, cloudflare, inventory, coverageOwnersFrom(cfg))
	if len(gaps) > 0 {
		for _, gap := range gaps {
			if !writeLine(stderr, "service-urls coverage: "+gap) {
				return 2
			}
		}
		return 1
	}
	count := len(repos.Active) + len(repos.Unmanaged)
	if !writeLine(stdout, fmt.Sprintf("service-urls coverage: complete (%d endpoints, %d repos)", len(endpoints), count)) {
		return 2
	}
	return 0
}

func coverageOwnersFrom(cfg config.Config) coverageOwners {
	return coverageOwners{
		kubernetes: cfg.Owners.Kubernetes,
		dns:        cfg.Owners.DNS,
		cloudflare: cfg.Owners.Cloudflare,
		workers:    cfg.Owners.Workers,
	}
}

func readCoverageInputs(root string, cfg config.Config) ([]endpoint, stringLists, stringLists, error) {
	var repos stringLists
	var cloudflare stringLists
	var network tailnet
	if err := readJSON(filepath.Join(root, cfg.ManagedReposPath), &repos); err != nil {
		return nil, stringLists{}, stringLists{}, fmt.Errorf("read managed repositories: %w", err)
	}
	if err := readJSON(filepath.Join(root, cfg.CloudflareHostsPath), &cloudflare); err != nil {
		return nil, stringLists{}, stringLists{}, fmt.Errorf("read Cloudflare hosts: %w", err)
	}
	tailnetData, err := os.ReadFile(filepath.Join(root, cfg.TailnetPath)) //nolint:gosec // G304: the root is the checked-out workspace root.
	if err != nil {
		return nil, stringLists{}, stringLists{}, fmt.Errorf("read tailnet: %w", err)
	}
	if err := toml.Unmarshal(tailnetData, &network); err != nil {
		return nil, stringLists{}, stringLists{}, fmt.Errorf("parse tailnet: %w", err)
	}
	var records []dnsRecord
	if err := readJSON(filepath.Join(root, cfg.DNSRecordsPath), &records); err != nil {
		return nil, stringLists{}, stringLists{}, fmt.Errorf("read internal DNS records: %w", err)
	}
	union, err := readUnion(filepath.Join(root, cfg.UnionPath))
	if err != nil {
		return nil, stringLists{}, stringLists{}, fmt.Errorf("read Kubernetes union: %w", err)
	}
	workerSource, err := os.ReadFile(filepath.Join(root, cfg.WorkerDomainsPath)) //nolint:gosec // G304: the root is the checked-out workspace root.
	if err != nil {
		return nil, stringLists{}, stringLists{}, fmt.Errorf("read Worker domains: %w", err)
	}
	kubernetes, err := k8sEndpoints(union, network, cfg)
	if err != nil {
		return nil, stringLists{}, stringLists{}, err
	}
	workers, err := workerDomainEndpoints(string(workerSource), cfg.Owners.Workers)
	if err != nil {
		return nil, stringLists{}, stringLists{}, err
	}
	endpoints := make([]endpoint, 0, len(kubernetes)+len(records)+len(cloudflare.Access)+len(cloudflare.Proxied)+len(workers))
	endpoints = append(endpoints, kubernetes...)
	endpoints = append(endpoints, dnsEndpoints(records, cfg)...)
	endpoints = append(endpoints, cloudflareEndpoints(cloudflare, cfg.Owners.Cloudflare)...)
	endpoints = append(endpoints, workers...)
	return endpoints, repos, cloudflare, nil
}

func readJSON(path string, target any) error {
	data, err := os.ReadFile(path) //nolint:gosec // Paths come from the deployment config.
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	return nil
}

func readUnion(path string) ([]any, error) {
	data, err := os.ReadFile(path) //nolint:gosec // The union path comes from the deployment config.
	if err != nil {
		return nil, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	documents := make([]any, 0)
	for {
		var value any
		if err := decoder.Decode(&value); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("decode YAML document: %w", err)
		}
		if value != nil {
			documents = append(documents, normalizeYAML(value))
		}
	}
	return documents, nil
}

func normalizeYAML(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			typed[key] = normalizeYAML(item)
		}
		return typed
	case map[any]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			result[fmt.Sprint(key)] = normalizeYAML(item)
		}
		return result
	case []any:
		for index, item := range typed {
			typed[index] = normalizeYAML(item)
		}
		return typed
	default:
		return value
	}
}
