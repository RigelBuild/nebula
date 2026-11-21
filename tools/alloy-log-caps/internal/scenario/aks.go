package scenario

import (
	"context"
	"fmt"
	"strings"

	"github.com/RigelBuild/nebula/tools/alloy-log-caps/internal/alloyrun"
	"github.com/RigelBuild/nebula/tools/alloy-log-caps/internal/echo"
)

// aksScenario checks the API-streamed source's stages: one bucket for every agent pod.
func (h *Harness) aksScenario(ctx context.Context) error {
	stages, err := yamlStage(h.AKSYAML, "spec", "values", "podLogsViaKubernetesApi", "extraLogProcessingStages")
	if err != nil {
		return err
	}
	r, err := alloyrun.Run(ctx, "aks", h.PodAlloy, "stage.cri {}", stages, aksStreams())
	if err != nil {
		return err
	}
	h.checkAKS(r)
	return nil
}

// aksStreams builds the agent-pool inputs: debug lines, a flood with a paced tail, and another pool.
func aksStreams() []alloyrun.Stream {
	debug := make([]string, 500)
	for i := range debug {
		debug[i] = fmt.Sprintf(`2026-10-19T00:00:00Z stdout F {"level":"debug","message":"poll %d"}`, i)
	}
	flood := make([]string, 8_000)
	pad := strings.Repeat(" x", 4096)
	for i := range flood {
		flood[i] = agentLine("f", i) + pad
	}
	sustain := make([]string, 40*sustainSeconds)
	for i := range sustain {
		sustain[i] = agentLine("-sustain", i)
	}
	dump := make([]string, 1_500)
	for i := range dump {
		dump[i] = agentLine("d", i)
	}
	other := make([]string, 2_000)
	for i := range other {
		other[i] = agentLine("o", i)
	}
	return []alloyrun.Stream{
		{Name: "debug", Labels: map[string]string{labelService: "pool-x86-agent"}, Lines: debug},
		{
			Name:   streamFlood,
			Labels: map[string]string{labelService: "pool-x86-agent"},
			Lines:  flood,
			// 2x the 20 lines/s cap, held after the pre-written flood spent the burst.
			Paced: &alloyrun.Paced{Lines: sustain, Dump: dump, PerSecond: 40},
		},
		{Name: "other", Labels: map[string]string{labelService: "pool-arm64-agent"}, Lines: other},
	}
}

func agentLine(tag string, i int) string {
	return fmt.Sprintf(`2026-10-19T00:00:00Z stdout F {"level":"info","message":"agent %s%d"}`, tag, i)
}

func (h *Harness) checkAKS(r echo.Result) {
	shipped := r.Lines[streamFlood]
	burstShipped := len(r.Lines["other"]) + countWithout(shipped, "-sustain")
	h.check(len(r.Lines["debug"]) == 0,
		fmt.Sprintf("aks: agent debug lines are dropped (shipped %d)", len(r.Lines["debug"])))
	h.check(float64(burstShipped) <= 6_000+20*r.Seconds,
		fmt.Sprintf("aks: all agent pods share one 6,000-line burst (shipped %d in %s s)",
			burstShipped, toFixed(r.Seconds, 1)))
	h.check(allOf(filterContains(shipped, " f"), func(l string) bool {
		return len(l) <= 4096 && strings.HasSuffix(l, "...[truncated]")
	}), "aks: oversized agent lines are cut to 4 KiB with the suffix")
	h.checkRate(r, streamFlood, "-sustain", 20, "aks: the agent pool")
}
