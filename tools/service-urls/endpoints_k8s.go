// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/RigelBuild/nebula/tools/service-urls/config"
)

type tailnet struct {
	MagicDNSSuffix string `toml:"magicDnsSuffix"`
	Nodes          map[string]struct {
		FQDN string `toml:"fqdn"`
	} `toml:"nodes"`
}

func k8sEndpoints(union []any, network tailnet, cfg config.Config) ([]endpoint, error) {
	resources := make([]map[string]any, 0)
	for _, document := range union {
		if err := collectResources(document, &resources); err != nil {
			return nil, err
		}
	}
	endpoints := make([]endpoint, 0)
	for _, resource := range resources {
		spec, ok := resource["spec"].(map[string]any)
		if !ok {
			continue
		}
		switch {
		case resource["kind"] == "Service" && spec["loadBalancerClass"] == "tailscale":
			found, err := serviceEndpoints(resource, spec, network, cfg.Owners.Kubernetes)
			if err != nil {
				return nil, err
			}
			endpoints = append(endpoints, found...)
		case resource["kind"] == "Ingress" && spec["ingressClassName"] == "tailscale":
			found, err := ingressEndpoints(resource, spec, network, cfg)
			if err != nil {
				return nil, err
			}
			endpoints = append(endpoints, found...)
		}
	}
	return endpoints, nil
}

func collectResources(value any, resources *[]map[string]any) error {
	resource, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	if resource["kind"] == "List" {
		items, ok := resource["items"].([]any)
		if !ok {
			return errors.New("kubernetes List has no items array")
		}
		for _, item := range items {
			if err := collectResources(item, resources); err != nil {
				return err
			}
		}
		return nil
	}
	*resources = append(*resources, resource)
	return nil
}

func serviceEndpoints(resource, spec map[string]any, network tailnet, owner string) ([]endpoint, error) {
	name, err := requiredResourceName(resource)
	if err != nil {
		return nil, err
	}
	ports, ok := spec["ports"].([]any)
	if !ok || len(ports) == 0 {
		return nil, fmt.Errorf("tailscale Service %s has no spec.ports", name)
	}
	hostname, ok := annotation(resource, "external-dns.alpha.kubernetes.io/hostname")
	if !ok {
		tailscaleName, found := annotation(resource, "tailscale.com/hostname")
		if !found {
			return nil, fmt.Errorf("tailscale Service %s has no hostname annotation", name)
		}
		hostname = fleetFQDN(tailscaleName, network)
	}
	endpoints := make([]endpoint, 0, len(ports))
	for index, item := range ports {
		portEntry, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tailscale Service %s port %d is invalid", name, index)
		}
		port, ok := numberValue(portEntry["port"])
		if !ok || port < 1 || port > 65535 {
			return nil, fmt.Errorf("tailscale Service %s port %d is invalid", name, index)
		}
		portValue := port
		endpoints = append(endpoints, endpoint{
			host:   strings.ToLower(hostname),
			port:   &portValue,
			origin: fmt.Sprintf("%s Service port %d", name, port),
			owner:  owner,
		})
	}
	return endpoints, nil
}

func ingressEndpoints(resource, spec map[string]any, network tailnet, cfg config.Config) ([]endpoint, error) {
	name, err := requiredResourceName(resource)
	if err != nil {
		return nil, err
	}
	tlsEntries, ok := spec["tls"].([]any)
	if !ok || len(tlsEntries) == 0 {
		return nil, fmt.Errorf("tailscale Ingress %s has no TLS hosts", name)
	}
	_, hasProxyGroup := annotation(resource, "tailscale.com/proxy-group")
	hosts := make([]string, 0)
	seen := make(map[string]struct{})
	for _, item := range tlsEntries {
		tls, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tailscale Ingress %s has an invalid TLS host list", name)
		}
		hostEntries, ok := tls["hosts"].([]any)
		if !ok || len(hostEntries) == 0 {
			return nil, fmt.Errorf("tailscale Ingress %s has an invalid TLS host list", name)
		}
		for _, hostValue := range hostEntries {
			host, ok := hostValue.(string)
			if !ok || strings.TrimSpace(host) == "" {
				return nil, fmt.Errorf("tailscale Ingress %s TLS host must be a non-empty string", name)
			}
			lower := strings.ToLower(host)
			tailscaleSuffix := "." + strings.ToLower(cfg.TailscaleDomain)
			if !hasProxyGroup && strings.HasSuffix(lower, tailscaleSuffix) {
				magicDNSSuffix := "." + strings.ToLower(network.MagicDNSSuffix)
				if !strings.HasSuffix(lower, magicDNSSuffix) {
					return nil, fmt.Errorf(
						"tailscale Ingress %s TLS host %q does not match network MagicDNS suffix %q",
						name,
						host,
						network.MagicDNSSuffix,
					)
				}
			}
			// A full tailscale-domain host names its tailnet already (for example
			// a personal tailnet), so only a short name takes the fleet suffix.
			if !strings.HasSuffix(lower, tailscaleSuffix) && !hasInternalSuffix(lower, cfg.InternalHostSuffixes) {
				lower = strings.ToLower(fleetFQDN(host, network))
			}
			if _, exists := seen[lower]; exists {
				continue
			}
			seen[lower] = struct{}{}
			hosts = append(hosts, lower)
		}
	}
	endpoints := make([]endpoint, 0, len(hosts))
	for _, host := range hosts {
		port := 443
		endpoints = append(endpoints, endpoint{
			host:   host,
			port:   &port,
			origin: name + " Ingress TLS host",
			owner:  cfg.Owners.Kubernetes,
		})
	}
	return endpoints, nil
}

// hasInternalSuffix reports whether host is one of the internal suffixes or
// lies under one. Each suffix is compared on a label boundary.
func hasInternalSuffix(host string, suffixes []string) bool {
	for _, suffix := range suffixes {
		if strings.HasSuffix(host, "."+strings.ToLower(suffix)) {
			return true
		}
	}
	return false
}

func requiredResourceName(resource map[string]any) (string, error) {
	metadata, ok := resource["metadata"].(map[string]any)
	if !ok {
		return "", errors.New("kubernetes resource has no metadata")
	}
	name, ok := metadata["name"].(string)
	if !ok || strings.TrimSpace(name) == "" {
		return "", errors.New("kubernetes resource metadata.name must be a non-empty string")
	}
	if namespace, ok := metadata["namespace"].(string); ok && namespace != "" {
		return namespace + "/" + name, nil
	}
	return name, nil
}

func annotation(resource map[string]any, key string) (string, bool) {
	metadata, ok := resource["metadata"].(map[string]any)
	if !ok {
		return "", false
	}
	annotations, ok := metadata["annotations"].(map[string]any)
	if !ok {
		return "", false
	}
	value, ok := annotations[key].(string)
	return value, ok && strings.TrimSpace(value) != ""
}

func numberValue(value any) (int, bool) {
	switch number := value.(type) {
	case int:
		return number, true
	case int64:
		if int64(int(number)) != number {
			return 0, false
		}
		return int(number), true
	case uint64:
		if number > uint64(math.MaxInt) {
			return 0, false
		}
		return int(number), true
	case float64:
		limit := math.Ldexp(1, strconv.IntSize-1)
		if number < -limit || number >= limit || number != float64(int(number)) {
			return 0, false
		}
		return int(number), true
	}
	return 0, false
}

func fleetFQDN(name string, network tailnet) string {
	if node, exists := network.Nodes[name]; exists && node.FQDN != "" {
		return node.FQDN
	}
	return name + "." + network.MagicDNSSuffix
}
