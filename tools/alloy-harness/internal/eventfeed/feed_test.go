package eventfeed

import (
	"strings"
	"testing"

	"github.com/RigelBuild/nebula/tools/alloy-harness/internal/check"
)

// checkNamed returns the assertion whose name starts with prefix. A missing name
// fails the test, so a renamed check cannot pass by absence.
func checkNamed(t *testing.T, checks []check.Check, prefix string) check.Check {
	t.Helper()
	for _, c := range checks {
		if strings.HasPrefix(c.Name, prefix) {
			return c
		}
	}
	t.Fatalf("no check named %q", prefix)
	return check.Check{}
}

// rendered mimics the host config shape the Pipeline slicer reads.
const rendered = `header
local.file_match "agent_events" {
  path_targets = [{__path__ = "/var/lib/agent-events/*.jsonl"}]
}
loki.source.file "agent_events" {
  forward_to = [otelcol.receiver.loki.logs.receiver]
}
// ── Enrich
tail
`

func TestPipelineSlicesRewritesAndEchoes(t *testing.T) {
	got, err := Pipeline(rendered, "/tmp/feed")
	if err != nil {
		t.Fatalf("Pipeline: %v", err)
	}
	if strings.Contains(got, "otelcol.receiver") {
		t.Errorf("pipeline still forwards to OTLP:\n%s", got)
	}
	if !strings.Contains(got, "forward_to = [loki.echo.e.receiver]") {
		t.Errorf("pipeline does not echo:\n%s", got)
	}
	if strings.Contains(got, "/var/lib/agent-events") {
		t.Errorf("pipeline still points at the real feed dir:\n%s", got)
	}
	if !strings.Contains(got, "/tmp/feed") {
		t.Errorf("pipeline does not point at the fixture dir:\n%s", got)
	}
	if strings.Contains(got, "tail") {
		t.Errorf("pipeline includes the enrich stage:\n%s", got)
	}
}

func TestPipelineRejectsMissingBlock(t *testing.T) {
	if _, err := Pipeline("no feed here", "/tmp/feed"); err == nil {
		t.Fatal("Pipeline accepted a config with no feed block")
	}
}

func TestPipelineRejectsBlockWithoutOTLPForward(t *testing.T) {
	cfg := "local.file_match \"agent_events\" {}\n// ── Enrich\n"
	if _, err := Pipeline(cfg, "/tmp/feed"); err == nil {
		t.Fatal("Pipeline accepted a feed block that does not forward to OTLP")
	}
}

func TestAssertPassesOnGoodRun(t *testing.T) {
	echoed := `ts=x receiver=loki.echo.e entry="dispatch:fixture:0" stream_id=\"agent-events\" unit=\"agent-events\"` + "\n" +
		strings.Repeat(`entry="dispatch:flood:x"`+"\n", 60)
	for _, c := range Assert(false, echoed) {
		if !c.OK {
			t.Errorf("check failed on a good run: %s", c.Name)
		}
	}
}

// TestAssertCatchesLeakedPrompt pins the rule that a prompt row in the echoed output
// fails the prompt check.
func TestAssertCatchesLeakedPrompt(t *testing.T) {
	echoed := "entry=\"dispatch:fixture:0\"\nentry=\"FIXTURE-PROMPT-MUST-NOT-SHIP\"\n"
	if checkNamed(t, Assert(false, echoed), "the prompt row").OK {
		t.Fatal("a leaked prompt row passed the prompt check")
	}
}

func TestAssertFloodBoundsBothWays(t *testing.T) {
	none := checkNamed(t, Assert(false, "entry=\"dispatch:fixture:0\"\n"), "stage.limit")
	if none.OK {
		t.Error("zero flood rows passed the stage.limit check")
	}
	all := strings.Repeat("dispatch:flood:x\n", floodRows)
	if checkNamed(t, Assert(false, "dispatch:fixture:0\n"+all), "stage.limit").OK {
		t.Error("every flood row passed the stage.limit check")
	}
}

func TestAssertFilenameLabel(t *testing.T) {
	echoed := `dispatch:fixture:0 unit=\"agent-events\" filename=\"/x\"`
	if checkNamed(t, Assert(false, echoed), "the feed row ships without a filename").OK {
		t.Error("a filename label passed the no-filename check")
	}
}

// TestAssertUnitLabelRequired pins the rule that the positive row passes the unit
// check only when it carries unit="agent-events".
func TestAssertUnitLabelRequired(t *testing.T) {
	without := `dispatch:fixture:0 stream_id=\"agent-events\"`
	if checkNamed(t, Assert(false, without), "the feed row ships with unit=agent-events").OK {
		t.Error("a row without unit=agent-events passed the unit check")
	}
	with := `dispatch:fixture:0 unit=\"agent-events\"`
	if !checkNamed(t, Assert(false, with), "the feed row ships with unit=agent-events").OK {
		t.Error("a row with unit=agent-events failed the unit check")
	}
}
