package scenario

import (
	"fmt"
	"math"
	"strings"

	"github.com/RigelBuild/nebula/tools/alloy-log-caps/internal/echo"
)

// checkRate asserts a stream ships about rate lines/s once its burst is spent.
// The span runs on Alloy log times from the line before the first tagged one.
func (h *Harness) checkRate(r echo.Result, stream, tag string, rate float64, what string) {
	all := r.Lines[stream]
	at := r.At[stream]
	first := -1
	var times []float64
	for i, l := range all {
		if !strings.Contains(l, tag) {
			continue
		}
		if first < 0 {
			first = i
		}
		times = append(times, at[i])
	}
	span := 0.0
	if first > 0 {
		span = times[len(times)-1] - at[first-1]
	}
	want := rate * span
	n := float64(len(times))
	ok := !math.IsNaN(span) && !math.IsInf(span, 0) && span > 0 &&
		n >= 0.8*want && n <= 1.2*want+2
	h.check(ok, fmt.Sprintf("%s: past its burst, ships ~%g lines/s (%d over %s s of Alloy time vs ~%s)",
		what, rate, len(times), toFixed(span, 1), toFixed(want, 0)))
}
