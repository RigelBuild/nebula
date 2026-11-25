// SPDX-License-Identifier: MIT OR Apache-2.0

package bundle

import (
	"path/filepath"
	"slices"
	"strings"
)

func sectionMatches(path string, section SectionRule) bool {
	if slices.Contains(section.Files, path) {
		return true
	}
	if section.Prefix == nil {
		return false
	}
	prefix := *section.Prefix
	return prefix == "" || path == prefix || strings.HasPrefix(path, strings.TrimSuffix(prefix, "/")+"/")
}

func packageAllowed(path string, rules PackageRules) bool {
	for _, entry := range rules.Allow {
		if strings.HasSuffix(entry, "/") {
			if strings.HasPrefix(path, entry) {
				return true
			}
		} else if path == entry {
			return true
		}
	}
	return false
}

func hasPackageReadmeDepth(path string, rules PackageRules) bool {
	parts := strings.Split(path, "/")
	if len(parts) < 2 || len(parts) > rules.ReadmeDepth {
		return false
	}
	return parts[len(parts)-1] == "README.md" || parts[len(parts)-1] == "AGENTS.md"
}

func pagefindValue(section *SectionRule) bool {
	return section == nil || section.Pagefind == nil || *section.Pagefind
}

func packageIdentifier(path string, nested []string) (string, string) {
	parts := strings.Split(path, "/")
	if len(parts) > 2 && slices.Contains(nested, parts[0]) {
		return parts[0] + "-" + parts[1], strings.Join(parts[2:], "/")
	}
	return parts[0], strings.Join(parts[1:], "/")
}

func pathWithinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
