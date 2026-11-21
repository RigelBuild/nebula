package scenario

import (
	"embed"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
)

// fixtures holds the captured log replays, embedded so the binary runs anywhere.
//
//go:embed testdata/*.txt
var fixtures embed.FS

// fixture reads one embedded replay file.
func fixture(name string) (string, error) {
	raw, err := fixtures.ReadFile("testdata/" + name)
	if err != nil {
		return "", fmt.Errorf("embedded fixture %s: %w", name, err)
	}
	return string(raw), nil
}

// lines splits fixture text, dropping trailing whitespace as JS trimEnd did.
func lines(text string) []string {
	return strings.Split(strings.TrimRightFunc(text, unicode.IsSpace), "\n")
}

// repeat cycles src out to n entries.
func repeat(src []string, n int) []string {
	out := make([]string, n)
	for i := range n {
		out[i] = src[i%len(src)]
	}
	return out
}

// padLines appends tag and 4 KiB of filler to each line, past the truncate cut.
func padLines(src []string, tag string) []string {
	out := make([]string, len(src))
	for i, l := range src {
		out[i] = l + tag + strings.Repeat(" x", 4096)
	}
	return out
}

func bytesOf(ls []string) int {
	n := 0
	for _, l := range ls {
		n += len(l)
	}
	return n
}

// toFixed formats x as JS Number.prototype.toFixed does: ties round away from
// zero, and NaN and infinities keep the JS spelling.
func toFixed(x float64, digits int) string {
	switch {
	case math.IsNaN(x):
		return "NaN"
	case math.IsInf(x, 1):
		return "Infinity"
	case math.IsInf(x, -1):
		return "-Infinity"
	}
	sign := ""
	if x < 0 {
		sign = "-"
		x = -x
	}
	scale := math.Pow10(digits)
	n := math.Floor(x*scale + 0.5)
	return sign + strconv.FormatFloat(n/scale, 'f', digits, 64)
}

// allOf reports whether every line satisfies ok. An empty set is true, as in JS every().
func allOf(ls []string, ok func(string) bool) bool {
	for _, l := range ls {
		if !ok(l) {
			return false
		}
	}
	return true
}

func filterContains(ls []string, sub string) []string {
	var out []string
	for _, l := range ls {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return out
}

func countWithout(ls []string, sub string) int {
	n := 0
	for _, l := range ls {
		if !strings.Contains(l, sub) {
			n++
		}
	}
	return n
}
