// SPDX-License-Identifier: MIT OR Apache-2.0

package bundle

import (
	"regexp"
	"strings"
)

var (
	githubSluggerRE          = regexp.MustCompile(githubSluggerPattern)
	fenceOpenPattern         = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})(.*)$")
	markdownExtensionPattern = regexp.MustCompile(`(?i)\.mdx?$`)
)

func RouteSlug(contentPath string) string {
	path := markdownExtensionPattern.ReplaceAllString(contentPath, "")
	parts := strings.Split(path, "/")
	for index, part := range parts {
		parts[index] = githubSlug(part)
	}
	return "/" + strings.TrimSuffix(strings.Join(parts, "/"), "/index")
}

func githubSlug(value string) string {
	value = strings.ReplaceAll(value, "İ", "i\u0307")
	value = strings.ToLower(value)
	value = githubSluggerRE.ReplaceAllString(value, "")
	return strings.ReplaceAll(value, " ", "-")
}

func closesFence(line string, marker byte, length int) bool {
	if length < 0 || marker == 0 {
		return false
	}
	index := 0
	for index < len(line) && index < 3 && line[index] == ' ' {
		index++
	}
	start := index
	for index < len(line) && line[index] == marker {
		index++
	}
	if index-start < length {
		return false
	}
	for ; index < len(line); index++ {
		if line[index] != ' ' && line[index] != '\t' {
			return false
		}
	}
	return true
}
