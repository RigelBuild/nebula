package echo

import (
	"math"
	"testing"
)

func TestMetricSumAddsSamplesOfOneName(t *testing.T) {
	metrics := "loki_process_dropped_lines_total{a=\"1\"} 3\n" +
		"loki_process_dropped_lines_total 4\n" +
		"other_metric 9\n"
	if got := MetricSum(metrics, "loki_process_dropped_lines_total"); got != 7 {
		t.Errorf("MetricSum = %v, want 7", got)
	}
}

func TestMetricSumNaNOnBadSample(t *testing.T) {
	metrics := "loki_process_dropped_lines_total 4\nloki_process_dropped_lines_total oops\n"
	if got := MetricSum(metrics, "loki_process_dropped_lines_total"); !math.IsNaN(got) {
		t.Errorf("MetricSum = %v, want NaN", got)
	}
}
