package jscompat

import (
	"math"
	"testing"
)

func TestNumberMatchesJavaScript(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want float64
	}{
		{"5368709120", 5368709120},
		{"  42  ", 42},
		{"", 0},
		{"   ", 0},
		{"0x10", 16},
		{"0o17", 15},
		{"0b101", 5},
		{"1e3", 1000},
		{".5", 0.5},
		{"-3", -3},
		{"+7", 7},
	}
	for _, c := range cases {
		if got := Number(c.in); got != c.want {
			t.Errorf("Number(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	for _, in := range []string{"abc", "12px", "0x", "Infinity", "1_000"} {
		if got := Number(in); !math.IsNaN(got) {
			t.Errorf("Number(%q) = %v, want NaN", in, got)
		}
	}
}

func TestIsSafeIntegerRejectsFractionsAndOverflow(t *testing.T) {
	t.Parallel()
	if !IsSafeInteger(5368709120) || !IsSafeInteger(0) {
		t.Error("expected safe integers to pass")
	}
	for _, v := range []float64{1.5, math.NaN(), math.Inf(1), 9007199254740992} {
		if IsSafeInteger(v) {
			t.Errorf("IsSafeInteger(%v) = true, want false", v)
		}
	}
}

func TestEncodeURIComponentMatchesJavaScript(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"example-dev":        "example-dev",
		"a b/c?d=e&f":        "a%20b%2Fc%3Fd%3De%26f",
		"id~!*'()_.-":        "id~!*'()_.-",
		"bucket-123":         "bucket-123",
		"S3-access-key":      "S3-access-key",
		"ünï":                "%C3%BCn%C3%AF",
		"secret-admin-token": "secret-admin-token",
	}
	for in, want := range cases {
		if got := EncodeURIComponent(in); got != want {
			t.Errorf("EncodeURIComponent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTrimRemovesJavaScriptWhitespace(t *testing.T) {
	t.Parallel()
	if got := Trim("\u00a0\tvalue\u2028\n"); got != "value" {
		t.Errorf("Trim = %q, want %q", got, "value")
	}
}
