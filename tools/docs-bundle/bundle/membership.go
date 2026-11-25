// SPDX-License-Identifier: MIT OR Apache-2.0

package bundle

import (
	"path"
	"strings"
)

const pullRequestTemplate = ".github/PULL_REQUEST_TEMPLATE.md"

func normalizePath(value string) string {
	return strings.TrimPrefix(strings.ReplaceAll(value, "\\", "/"), "./")
}

func isSkippedDirectory(name string) bool {
	return name != ".github" && (name == "node_modules" || name == "dist" || name == "outputs" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_"))
}

// IsGatherable reports whether a repository-relative Markdown file is collected.
func IsGatherable(value string) bool {
	normalized := normalizePath(value)
	if !strings.HasSuffix(strings.ToLower(normalized), ".md") {
		return false
	}
	segments := strings.Split(normalized, "/")
	for index, segment := range segments {
		if index < len(segments)-1 && isSkippedDirectory(segment) {
			return false
		}
		if (strings.HasPrefix(segment, ".") || strings.HasPrefix(segment, "_")) && normalized != pullRequestTemplate {
			return false
		}
	}
	if strings.HasPrefix(normalized, "docs/designs/") || strings.EqualFold(path.Base(normalized), "CHANGELOG.md") {
		return false
	}
	return true
}
