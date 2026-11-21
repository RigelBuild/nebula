package scenario

import (
	"io"
	"math"
	"testing"

	"github.com/RigelBuild/nebula/tools/alloy-log-caps/internal/echo"
)

// burstLines is one prior line at t=0, then n tagged lines spread over span seconds.
func burstLines(n int, span float64) ([]string, []float64) {
	ls := []string{"burst"}
	at := []float64{0}
	for i := 1; i <= n; i++ {
		ls = append(ls, "x -sustain")
		at = append(at, span*float64(i)/float64(n))
	}
	return ls, at
}

func TestCheckRate(t *testing.T) {
	// Rate 10 over a 10 s span wants 100 lines, so the band is [80, 122].
	inBand, inAt := burstLines(100, 10)
	low, lowAt := burstLines(79, 10)
	high, highAt := burstLines(123, 10)
	noPrior := []string{"x -sustain", "x -sustain"}
	noPriorAt := []float64{1, 2}
	nan := []string{"burst", "x -sustain"}
	nanAt := []float64{0, math.NaN()}
	cases := []struct {
		name   string
		lines  []string
		at     []float64
		wantOK bool
	}{
		{"in band", inBand, inAt, true},
		{"below 0.8x", low, lowAt, false},
		{"above 1.2x+2", high, highAt, false},
		{"first tagged line has no prior line", noPrior, noPriorAt, false},
		{"NaN timestamp", nan, nanAt, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &Harness{Out: io.Discard}
			r := echo.Result{
				Lines: map[string][]string{"s": c.lines},
				At:    map[string][]float64{"s": c.at},
			}
			h.checkRate(r, "s", "-sustain", 10, "case")
			if got := len(h.failures) == 0; got != c.wantOK {
				t.Errorf("checkRate ok = %v, want %v", got, c.wantOK)
			}
		})
	}
}
