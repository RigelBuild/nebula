// Package eventfeed checks the rendered agent-events pipeline in a real Alloy.
package eventfeed

import (
	"errors"
	"fmt"
	"strings"

	"github.com/RigelBuild/nebula/tools/alloy-harness/internal/check"
)

const (
	blockStart      = `local.file_match "agent_events"`
	blockEnd        = "// ── Enrich"
	forwardToOTLP   = "forward_to = [otelcol.receiver.loki.logs.receiver]"
	echoForward     = "forward_to = [loki.echo.e.receiver]"
	renderedFeedDir = "/var/lib/agent-events"
	dispatchMarker  = "dispatch:fixture:0"
	promptMarker    = "FIXTURE-PROMPT-MUST-NOT-SHIP"
	ledgerMarker    = "LEDGER-SENTINEL-MUST-NOT-SHIP"
	floodMarker     = "dispatch:flood:"
	floodRows       = 120
	unitLabel       = `unit="agent-events"`
	filenameLabel   = "filename="
)

// Pipeline slices the feed block out of the rendered host config. The block ends
// at the enrich stage. It points the feed at feedDir and echoes instead of exporting.
func Pipeline(rendered, feedDir string) (string, error) {
	start := strings.Index(rendered, blockStart)
	end := strings.Index(rendered, blockEnd)
	if start < 0 || end <= start {
		return "", errors.New("event feed pipeline not found in rendered config")
	}
	block := rendered[start:end]
	if !strings.Contains(block, forwardToOTLP) {
		return "", errors.New("event feed pipeline does not forward to the OTLP receiver")
	}
	out := strings.Replace(block, forwardToOTLP, echoForward, 1)
	return strings.ReplaceAll(out, renderedFeedDir, feedDir), nil
}

// ConfigFile is the Alloy config the harness writes: the feed block plus its echo sink.
func ConfigFile(pipeline string) string {
	return pipeline + "\nloki.echo \"e\" {}\n"
}

// Done reports whether the positive row has been echoed yet.
func Done(echoed string) bool {
	return strings.Contains(echoed, dispatchMarker)
}

// Assert evaluates the feed assertions over one run. exited is whether Alloy
// exited early, and echoed is its full echoed output.
func Assert(exited bool, echoed string) []check.Check {
	flood := strings.Count(echoed, floodMarker)
	labelLine := labelLineOf(echoed)
	return []check.Check{
		{Name: "Alloy stayed up on the event feed pipeline (no config error)", OK: !exited},
		{Name: "the dispatch row ships through the event feed (positive control)", OK: strings.Contains(echoed, dispatchMarker)},
		{Name: "the prompt row never reaches the event feed output", OK: !strings.Contains(echoed, promptMarker)},
		{Name: "a ledger-tree sentinel outside the feed dir never reaches the output", OK: !strings.Contains(echoed, ledgerMarker)},
		{Name: fmt.Sprintf("stage.limit caps the flood (%d of %d rows shipped)", flood, floodRows), OK: flood > 0 && flood < floodRows},
		{Name: "the feed row ships with unit=agent-events", OK: strings.Contains(labelLine, unitLabel)},
		{Name: "the feed row ships without a filename label", OK: !strings.Contains(labelLine, filenameLabel)},
	}
}

// labelLineOf returns the echoed line for the positive row, with backslashes removed.
// Alloy's echo escapes quotes inside labels=, so the checks compare the unescaped text.
func labelLineOf(echoed string) string {
	for line := range strings.SplitSeq(echoed, "\n") {
		if strings.Contains(line, dispatchMarker) {
			return strings.ReplaceAll(line, `\`, "")
		}
	}
	return ""
}
