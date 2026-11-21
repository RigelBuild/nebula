// Package jscompat reproduces the JavaScript string and number rules the admin tooling's config parsing relies on.
package jscompat

import (
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// maxSafeInteger is Number.MAX_SAFE_INTEGER, 2^53-1.
const maxSafeInteger = 9007199254740991

// decimalLiteral is the StrDecimalLiteral form that Number() accepts without a radix prefix.
var decimalLiteral = regexp.MustCompile(`^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$`)

// isSpace reports whether r is a character that String.prototype.trim removes.
func isSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', 0x2028, 0x2029, 0xFEFF:
		return true
	}
	return unicode.Is(unicode.Zs, r)
}

// Trim mirrors String.prototype.trim.
func Trim(s string) string {
	return strings.TrimFunc(s, isSpace)
}

// Number mirrors Number(s) for decimal, 0x, 0o and 0b forms. It returns NaN where JavaScript does.
func Number(s string) float64 {
	t := Trim(s)
	if t == "" {
		return 0
	}
	if len(t) > 2 && t[0] == '0' {
		base := 0
		switch t[1] {
		case 'x', 'X':
			base = 16
		case 'o', 'O':
			base = 8
		case 'b', 'B':
			base = 2
		}
		if base != 0 {
			value, err := strconv.ParseUint(t[2:], base, 64)
			if err != nil {
				return math.NaN()
			}
			return float64(value)
		}
	}
	if !decimalLiteral.MatchString(t) {
		return math.NaN()
	}
	value, err := strconv.ParseFloat(t, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return math.NaN()
	}
	return value
}

// IsSafeInteger mirrors Number.isSafeInteger.
func IsSafeInteger(value float64) bool {
	return value == math.Trunc(value) && math.Abs(value) <= maxSafeInteger
}

// EncodeURIComponent mirrors encodeURIComponent: every byte outside the unreserved set is percent-encoded.
func EncodeURIComponent(s string) string {
	const upperHex = "0123456789ABCDEF"
	var out strings.Builder
	for i := range len(s) {
		c := s[i]
		if isUnreserved(c) {
			out.WriteByte(c)
			continue
		}
		out.WriteByte('%')
		out.WriteByte(upperHex[c>>4])
		out.WriteByte(upperHex[c&0x0F])
	}
	return out.String()
}

// isUnreserved reports whether encodeURIComponent leaves c as is.
func isUnreserved(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("-_.!~*'()", c) >= 0
}
