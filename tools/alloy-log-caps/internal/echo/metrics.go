package echo

import (
	"math"
	"strconv"
	"strings"
)

// MetricSum adds every sample of one metric name. An unparseable value makes
// the sum NaN, so the drain loop keeps waiting, as the JS Number() did.
func MetricSum(metrics, name string) float64 {
	var sum float64
	for l := range strings.SplitSeq(metrics, "\n") {
		if !strings.HasPrefix(l, name+"{") && !strings.HasPrefix(l, name+" ") {
			continue
		}
		fields := strings.Split(l, " ")
		v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			v = math.NaN()
		}
		sum += v
	}
	return sum
}
