package scenario

import (
	"context"
	"fmt"
	"strings"

	"github.com/RigelBuild/nebula/tools/alloy-log-caps/internal/alloyrun"
	"github.com/RigelBuild/nebula/tools/alloy-log-caps/internal/echo"
)

// hostScenario checks the rendered journal stages, including per-unit line filters.
func (h *Harness) hostScenario(ctx context.Context) error {
	stages, err := readHostStages(h.HostConfig)
	if err != nil {
		return err
	}
	spikeText, err := fixture("postgres-test-spike.journal.txt")
	if err != nil {
		return err
	}
	streams, p99 := hostStreams(lines(spikeText))
	r, err := alloyrun.Run(ctx, "host", h.HostAlloy, "", stages, streams)
	if err != nil {
		return err
	}
	h.checkHost(r, len(p99))
	return nil
}

// hostStreams builds the spiking unit and litellm's access log. It also returns
// the p99 lines, which arrive paced after a dump empties litellm's burst.
func hostStreams(spikeSrc []string) ([]alloyrun.Stream, []string) {
	p99 := make([]string, 1_353/3)
	for i := range p99 {
		p99[i] = accessLine("p99-", i)
	}
	dumpAccess := make([]string, 4_000)
	for i := range dumpAccess {
		dumpAccess[i] = accessLine("dump-", i)
	}
	streams := []alloyrun.Stream{
		{
			Name:   "spike",
			Labels: map[string]string{"unit": "pgtest.service"},
			Lines:  repeat(padLines(spikeSrc, ""), 50_007),
			// 2.5x the 32 lines/s per-unit cap, held after the burst is spent.
			Paced: &alloyrun.Paced{
				Lines:     repeat(padLines(spikeSrc, " sustain"), 80*sustainSeconds),
				Dump:      repeat(padLines(spikeSrc, ""), 2_500),
				PerSecond: 80,
			},
		},
		{
			Name:   "litellm",
			Labels: map[string]string{"unit": "podman-litellm.service"},
			Lines:  dumpAccess,
			Paced:  &alloyrun.Paced{Lines: p99, PerSecond: 1_353.0 / 60},
		},
	}
	return streams, p99
}

func accessLine(tag string, i int) string {
	return fmt.Sprintf(`INFO: 10.0.0.1 - "POST /v1/messages HTTP/1.1" 500 Internal Server Error %s%d`, tag, i)
}

// checkHost asserts the spike budget and truncate, the litellm dump, and the p99 rate.
func (h *Harness) checkHost(r echo.Result, p99Want int) {
	budget := 1.05*mb + 32*512*r.Seconds
	litellm := r.Lines["litellm"]
	p99Shipped := len(filterContains(litellm, "p99-"))
	h.check(float64(bytesOf(r.Lines["spike"])) <= budget,
		fmt.Sprintf("host: one unit's spike ships <= ~1 MB burst + 16 KiB/s (shipped %d B in %s s)",
			bytesOf(r.Lines["spike"]), toFixed(r.Seconds, 1)))
	h.check(len(r.Lines["spike"]) > 0, "host: the spiking unit still ships some lines")
	h.check(allOf(r.Lines["spike"], func(l string) bool {
		return len(l) <= 512 && strings.HasSuffix(l, "...[truncated]")
	}), "host: oversized lines are cut to 512 B with the suffix")
	h.check(len(litellm) < 4_000,
		fmt.Sprintf("host: litellm's dump empties its burst (shipped %d/4000)", len(litellm)-p99Shipped))
	h.check(p99Shipped == p99Want,
		fmt.Sprintf("host: litellm's p99 rate ships whole once its burst is gone (%d/%d over %s s)",
			p99Shipped, p99Want, toFixed(r.Seconds, 1)))
	h.checkRate(r, "spike", " sustain ", 32, "host: a spiking unit")
}
