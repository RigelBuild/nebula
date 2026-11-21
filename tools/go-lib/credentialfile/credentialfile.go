// SPDX-License-Identifier: MIT OR Apache-2.0

// Package credentialfile reads non-empty trimmed credentials from files.
package credentialfile

import (
	"os"
	"strings"
	"unicode"
)

func trimECMAScriptWhitespace(value string) string {
	return strings.TrimFunc(value, func(r rune) bool {
		if r == '\u0085' {
			return false
		}
		return unicode.IsSpace(r) || r == '\ufeff'
	})
}

// Read returns the trimmed contents of a readable non-empty file.
func Read(path string) (string, bool) {
	if trimECMAScriptWhitespace(path) == "" {
		return "", false
	}
	contents, err := os.ReadFile(path) //nolint:gosec // The caller supplies a credential-file path intentionally.
	if err != nil {
		return "", false
	}
	value := trimECMAScriptWhitespace(string(contents))
	if value == "" {
		return "", false
	}
	return value, true
}
