// SPDX-License-Identifier: MIT OR Apache-2.0

// Package apiurl resolves the Woodpecker API base URL for CI tools running
// inside or outside the cluster.
package apiurl

import "strings"

// WoodpeckerAPI returns the Woodpecker API base URL from env, or false when no
// source is set. Precedence: WOODPECKER_API_URL, then inCluster when
// KUBERNETES_SERVICE_HOST is set, then CI_SYSTEM_URL. inCluster is the API
// address the caller reaches from inside the cluster; an empty inCluster is
// skipped. Trailing slashes are stripped.
func WoodpeckerAPI(env func(string) string, inCluster string) (string, bool) {
	if explicit := strings.TrimSpace(env("WOODPECKER_API_URL")); explicit != "" {
		return strings.TrimRight(explicit, "/"), true
	}
	if inCluster = strings.TrimSpace(inCluster); inCluster != "" && strings.TrimSpace(env("KUBERNETES_SERVICE_HOST")) != "" {
		return strings.TrimRight(inCluster, "/"), true
	}
	if system := strings.TrimSpace(env("CI_SYSTEM_URL")); system != "" {
		return strings.TrimRight(system, "/"), true
	}
	return "", false
}
