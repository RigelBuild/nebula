// Package scrapelimit checks that Alloy's sample_limit refuses an over-limit scrape and
// passes a scrape at the limit.
package scrapelimit

import (
	"fmt"
	"strconv"
	"strings"
)

// Exceeded is the Alloy counter for scrapes refused over sample_limit.
const Exceeded = "prometheus_target_scrapes_exceeded_sample_limit_total"

// MinHits is how many scrapes the fixture must answer before a verdict.
const MinHits = 3

// Body is the /metrics page the fixture serves: one gauge series per sample, so the
// sample count is exact.
func Body(samples int) string {
	lines := make([]string, samples)
	for i := range samples {
		lines[i] = fmt.Sprintf(`fixture_sample{i="%d"} 1`, i)
	}
	return strings.Join(lines, "\n") + "\n"
}

// Config is the Alloy config that scrapes the fixture on targetPort with sample_limit.
func Config(targetPort, limit int) string {
	return fmt.Sprintf(`prometheus.scrape "fixture" {
  targets         = [{ __address__ = "127.0.0.1:%d", job = "fixture" }]
  forward_to      = [prometheus.remote_write.sink.receiver]
  scrape_interval = "1s"
  scrape_timeout  = "1s"
  sample_limit    = %d
}

prometheus.remote_write "sink" {
  endpoint {
    url = "http://127.0.0.1:1/api/v1/push"
  }
}
`, targetPort, limit)
}

// ExceededTotal sums every Exceeded series in a Prometheus text page. A value that does
// not parse as a number counts as zero.
func ExceededTotal(page string) float64 {
	var total float64
	for line := range strings.SplitSeq(page, "\n") {
		if !strings.HasPrefix(line, Exceeded+" ") && !strings.HasPrefix(line, Exceeded+"{") {
			continue
		}
		fields := strings.Fields(line)
		v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			continue
		}
		total += v
	}
	return total
}

// Settled reports whether a run has enough evidence for a verdict: the fixture answered
// MinHits scrapes, and either the target is within the limit or Alloy counted a refusal.
func Settled(samples, limit int, hits int64, exceeded float64) bool {
	return hits >= MinHits && (samples <= limit || exceeded >= 1)
}

// ScrapeLines returns the scrape-related series in a page, for a failure message.
func ScrapeLines(page string) string {
	var keep []string
	for line := range strings.SplitSeq(page, "\n") {
		if strings.Contains(line, "scrape") && !strings.HasPrefix(line, "#") {
			keep = append(keep, line)
		}
	}
	return strings.Join(keep, "\n")
}
