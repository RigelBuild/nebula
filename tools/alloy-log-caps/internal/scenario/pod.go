package scenario

import (
	"context"
	"fmt"
	"strings"

	"github.com/RigelBuild/nebula/tools/alloy-log-caps/internal/alloyrun"
	"github.com/RigelBuild/nebula/tools/alloy-log-caps/internal/echo"
)

// podScenario checks the pod-log stages: the chart's CRI parse, then the shipped extra stages.
func (h *Harness) podScenario(ctx context.Context) error {
	stages, err := yamlStage(h.PodYAML, "spec", "values", "podLogsViaLoki", "extraLogProcessingStages")
	if err != nil {
		return err
	}
	replayText, err := fixture("longhorn-replay.cri.txt")
	if err != nil {
		return err
	}
	replay := lines(replayText)
	quiet := quietLines(338)
	r, err := alloyrun.Run(ctx, "pod", h.PodAlloy, "stage.cri {}", stages, podStreams(replay, quiet))
	if err != nil {
		return err
	}
	h.checkPod(r, quiet)
	return nil
}

// podStreams builds the pod inputs: Longhorn replay, a flood, and a quiet service.
func podStreams(replay []string, quiet []string) []alloyrun.Stream {
	return []alloyrun.Stream{
		{Name: "replay", Labels: map[string]string{labelService: "longhorn"}, Lines: repeat(replay, 20_000)},
		{
			Name:   streamFlood,
			Labels: map[string]string{labelService: streamFlood},
			Lines:  repeat(podFlood(replay, ""), 20_000),
			// 2.5x the 16 lines/s cap, held after the pre-written flood spent the burst.
			Paced: &alloyrun.Paced{
				Lines:     repeat(podFlood(replay, "-sustain"), 40*sustainSeconds),
				Dump:      repeat(podFlood(replay, ""), 1_500),
				PerSecond: 40,
			},
		},
		{Name: "quiet", Labels: map[string]string{labelService: "kyverno-admission-controller"}, Lines: quiet},
	}
}

// podFlood renames the replay marker and pads each line past the 1 KiB cut.
// The drop misses it, so only the truncate keeps the flood near 1 MB.
func podFlood(replay []string, tag string) []string {
	pad := strings.Repeat(" x", 4096)
	out := make([]string, len(replay))
	for i, l := range replay {
		out[i] = strings.Replace(l, "printInstanceLogs", "syncEngine"+tag, 1) + pad
	}
	return out
}

func quietLines(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf(`2026-10-15T17:00:00.%09dZ stdout F level=info msg="reconcile %d"`, i, i)
	}
	return out
}

func (h *Harness) checkPod(r echo.Result, quiet []string) {
	budget := 1.05*mb + 16*1024*r.Seconds
	h.check(len(r.Lines["replay"]) == 0,
		fmt.Sprintf("pod: captured Longhorn replay lines are dropped (shipped %d)", len(r.Lines["replay"])))
	h.check(float64(bytesOf(r.Lines[streamFlood])) <= budget,
		fmt.Sprintf("pod: one service_name flood ships <= ~1 MB burst + 16 KiB/s (shipped %d B in %s s)",
			bytesOf(r.Lines[streamFlood]), toFixed(r.Seconds, 1)))
	h.check(len(r.Lines[streamFlood]) > 0, "pod: the flooding service still ships some lines")
	h.check(allOf(r.Lines[streamFlood], func(l string) bool {
		return len(l) <= 1024 && strings.HasSuffix(l, "...[truncated]")
	}), "pod: oversized lines are cut to 1 KiB with the suffix")
	h.check(len(r.Lines["quiet"]) == len(quiet),
		fmt.Sprintf("pod: a normal service's peak minute ships whole (%d/%d)", len(r.Lines["quiet"]), len(quiet)))
	h.checkRate(r, streamFlood, "-sustain", 16, "pod: a flooding service_name")
}
