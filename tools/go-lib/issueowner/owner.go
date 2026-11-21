// SPDX-License-Identifier: MIT OR Apache-2.0

package issueowner

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	OwnerDiagnosticMalformed = "malformed-owner-line"
	LegacyWindow             = 500
)

var (
	// jsSpace matches ECMAScript whitespace rather than RE2's ASCII-only \s.
	jsSpace                  = `\x{0009}-\x{000D}\x{0020}\x{00A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}\x{FEFF}`
	OwnerCanonical           = regexp.MustCompile(`(^|[\r\n\x{2028}\x{2029}])\*{0,2}Owner:\*{0,2}[ \t]+([a-z][a-z0-9-]*)([` + jsSpace + `.,;()/*]|$)`)
	OwnerLegacy              = regexp.MustCompile(`(^|[.\x{0009}-\x{000D}\x{0020}\x{00A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}\x{FEFF}*])\*{0,2}Owner:\*{0,2}[ \t]+([a-z][a-z0-9-]*)([` + jsSpace + `.,;()/*]|$)`)
	LeadingBlockEnd          = regexp.MustCompile(`\n[ \t]*\n`)
	OwnerProbe               = regexp.MustCompile(`(?i)(^|[\r\n\x{2028}\x{2029}])[ \t]*owner[ \t]*:[ \t]*([^\r\n\x{2028}\x{2029}]*)`)
	OwnerToken               = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	ownerClaimTokenSplitter  = regexp.MustCompile(`[` + jsSpace + `(]`)
	ownerTrailingPunctuation = regexp.MustCompile(`[.,]+$`)
	ownerJSWhitespaceTrim    = regexp.MustCompile(`^[` + jsSpace + `]+|[` + jsSpace + `]+$`)
)

// IsStateType reports whether value is one of the seam's five state values.
func IsStateType(value string) bool {
	switch StateType(value) {
	case StateBacklog, StateUnstarted, StateStarted, StateCompleted, StateCanceled:
		return true
	default:
		return false
	}
}

// ReadOwnerFromBody returns a parsed owner token, if present.
func ReadOwnerFromBody(body string) (string, bool) {
	for _, match := range OwnerCanonical.FindAllStringSubmatchIndex(body, -1) {
		if ownerTerminated(body, match[5]) {
			return body[match[4]:match[5]], true
		}
	}
	for _, match := range OwnerLegacy.FindAllStringSubmatchIndex(body, -1) {
		if utf16Length(body[:match[0]]) < LegacyWindow && ownerTerminated(body, match[5]) {
			return body[match[4]:match[5]], true
		}
	}
	return "", false
}

func ownerTerminated(body string, end int) bool {
	if end == len(body) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(body[end:])
	return strings.ContainsRune(".,;()/*", r) || isJSWhitespace(r)
}

func isJSWhitespace(r rune) bool {
	return r >= '\t' && r <= '\r' || r == ' ' || r == '\u00a0' || r == '\u1680' ||
		r >= '\u2000' && r <= '\u200a' || r == '\u2028' || r == '\u2029' ||
		r == '\u202f' || r == '\u205f' || r == '\u3000' || r == '\ufeff'
}

func trimJSSpace(value string) string {
	return ownerJSWhitespaceTrim.ReplaceAllString(value, "")
}

// ReadOwnerDiagnosticFromBody reports a malformed owner-shaped line.
func ReadOwnerDiagnosticFromBody(body string) (Diagnostic, bool) {
	if _, ok := ReadOwnerFromBody(body); ok {
		return Diagnostic{}, false
	}
	match := OwnerProbe.FindStringSubmatch(body)
	if match == nil {
		return Diagnostic{}, false
	}
	line := trimJSSpace(match[0])
	claimed := trimJSSpace(match[2])
	claimed = ownerClaimTokenSplitter.Split(claimed, 2)[0]
	claimed = ownerTrailingPunctuation.ReplaceAllString(claimed, "")
	if claimed == "" {
		return Diagnostic{Kind: OwnerDiagnosticMalformed, Line: line, Detail: "owner line has no token after the colon"}, true
	}
	lower := strings.ToLower(claimed)
	if lower != claimed {
		return Diagnostic{
			Kind:   OwnerDiagnosticMalformed,
			Line:   line,
			Detail: `owner token ` + quoteJS(claimed) + ` is not lowercase; agent tokens are lowercase kebab (` + quoteJS(lower) + `)`,
		}, true
	}
	if !OwnerToken.MatchString(claimed) {
		return Diagnostic{Kind: OwnerDiagnosticMalformed, Line: line, Detail: `owner token ` + quoteJS(claimed) + ` is not [a-z][a-z0-9-]*`}, true
	}
	return Diagnostic{Kind: OwnerDiagnosticMalformed, Line: line, Detail: `owner line is not in canonical form (expected "Owner: ` + claimed + `")`}, true
}

func quoteJS(value string) string {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return `""`
	}
	quoted := strings.TrimSuffix(encoded.String(), "\n")
	quoted = strings.ReplaceAll(quoted, "\\u2028", " ")
	quoted = strings.ReplaceAll(quoted, "\\u2029", " ")
	return quoted
}

func utf16Length(value string) int {
	length := 0
	for len(value) > 0 {
		r, size := utf8.DecodeRuneInString(value)
		if r > 0xffff {
			length += 2
		} else {
			length++
		}
		value = value[size:]
	}
	return length
}

// WriteOwnerInBody writes a canonical owner line and only removes owner lines in the leading block.
func WriteOwnerInBody(body, owner string) string {
	separator := LeadingBlockEnd.FindStringIndex(body)
	leading := body
	tail := ""
	if separator != nil {
		leading = body[:separator[0]]
		tail = body[separator[0]:]
	}
	lines := strings.Split(leading, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if !isCanonicalOwnerLine(line) {
			kept = append(kept, line)
		}
	}
	var remainder string
	if len(kept) == 0 {
		if separator != nil {
			remainder = body[separator[1]:]
		}
	} else {
		remainder = strings.Join(kept, "\n") + tail
	}
	header := "Owner: " + owner
	if remainder == "" {
		return header
	}
	return header + "\n\n" + remainder
}

func isCanonicalOwnerLine(line string) bool {
	match := OwnerCanonical.FindStringSubmatchIndex(line)
	return match != nil && ownerTerminated(line, match[5])
}
