// SPDX-License-Identifier: MIT OR Apache-2.0

package issueowner

import (
	"slices"
	"strings"
)

// ReservedCodenames cannot identify an agent branch owner.
var ReservedCodenames = []string{
	"mintaka",
	"seal",
	"renovate",
	"dependabot",
	"trunk-merge",
	"graphite-base",
}

// IsReservedCodename reports whether codename is reserved, case-insensitively.
func IsReservedCodename(codename string) bool {
	return slices.Contains(ReservedCodenames, strings.ToLower(codename))
}
