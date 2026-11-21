package scenario

import (
	"strings"
	"testing"
)

func TestToFixedMatchesJS(t *testing.T) {
	cases := []struct {
		x      float64
		digits int
		want   string
	}{
		{2.5, 0, "3"},
		{1.25, 1, "1.3"},
		{16.0, 0, "16"},
		{1.04, 1, "1.0"},
	}
	for _, c := range cases {
		if got := toFixed(c.x, c.digits); got != c.want {
			t.Errorf("toFixed(%v, %d) = %q, want %q", c.x, c.digits, got, c.want)
		}
	}
}

func TestLinesDropsTrailingWhitespace(t *testing.T) {
	got := lines("a\nb\n\n")
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("lines = %q, want [a b]", got)
	}
}

func TestRepeatCycles(t *testing.T) {
	got := repeat([]string{"x", "y"}, 5)
	want := []string{"x", "y", "x", "y", "x"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("repeat = %q, want %q", got, want)
	}
}
