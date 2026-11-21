package seat

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// startSeatArgs is the argv every pane test starts a seat with.
var startSeatArgs = []string{"start", "--name", "seat", "--", "omp"}

// lastCallIsStop reports whether the final spawn was the systemd stop of the test unit.
func lastCallIsStop(calls []recordedCall) bool {
	if len(calls) == 0 {
		return false
	}
	last := calls[len(calls)-1]
	return last.command == systemctlCommand && slices.Equal(last.args, []string{"--user", "stop", "test-seat-seat"})
}

// A launcher that dies at once is reported with its cause and last pane lines, and the unit is stopped.
func TestPaneDeadAtOnceReportsCauseAndStopsTheUnit(t *testing.T) {
	tests := []struct {
		name   string
		status string
		signal string
		reason string
	}{
		{name: "exit status", status: "3", signal: "", reason: "exited with status 3"},
		{name: "signal", status: "", signal: "9", reason: "killed by signal 9"},
		{name: "empty status", status: "", signal: "", reason: "exited with status unknown"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const output = "ReferenceError: require is not defined"
			fake := fakeDeps(fakeOptions{deadPane: &deadPane{status: test.status, signal: test.signal, output: output + "\n"}})
			assertDeepEqual(t, Run(context.Background(), startSeatArgs, fake.deps), 1)
			joined := strings.Join(fake.errors, " ")
			if !strings.Contains(joined, test.reason) || !strings.Contains(joined, output) {
				t.Fatalf("errors = %q, want %q and %q", fake.errors, test.reason, output)
			}
			// The output is read before the unit is stopped, and the stop comes last.
			captured := -1
			for i, call := range fake.calls {
				if slices.Contains(call.args, "capture-pane") {
					captured = i
					break
				}
			}
			if captured < 0 {
				t.Fatal("capture-pane was never called")
			}
			assertDeepEqual(t, fake.calls[captured].args[len(fake.calls[captured].args)-2:], []string{"-S", "-20"})
			if !lastCallIsStop(fake.calls) {
				t.Fatalf("last call = %+v, want the unit stop", fake.calls[len(fake.calls)-1])
			}
		})
	}
}

// A failed remain-on-exit, on or off, stops the unit and reports it.
func TestPaneFailedRemainOnExitStopsTheUnit(t *testing.T) {
	for _, value := range []string{"on", "off"} {
		t.Run(value, func(t *testing.T) {
			fake := fakeDeps(fakeOptions{setOptionFails: value})
			assertDeepEqual(t, Run(context.Background(), startSeatArgs, fake.deps), 1)
			assertContainsLine(t, fake.errors, "tmux set-option failed: set failed")
			if !lastCallIsStop(fake.calls) {
				t.Fatalf("last call = %+v, want the unit stop", fake.calls[len(fake.calls)-1])
			}
		})
	}
}

// A failed #{pane_dead} display stops the unit and reports it.
func TestPaneFailedPaneDeadDisplayStopsTheUnit(t *testing.T) {
	fake := fakeDeps(fakeOptions{paneDeadDisplayErr: "no current client"})
	assertDeepEqual(t, Run(context.Background(), startSeatArgs, fake.deps), 1)
	assertDeepEqual(t, fake.errors, []string{"wave-seat-holder: tmux display failed: no current client\n"})
	if !lastCallIsStop(fake.calls) {
		t.Fatalf("last call = %+v, want the unit stop", fake.calls[len(fake.calls)-1])
	}
}

// tmux 3.4 keeps pane_pid after the launcher dies and drops its children file. A dead pane must still be
// reported as dead, not waited on until the startup timeout.
func TestPaneDeadWhileTmuxKeepsThePanePIDReportsCauseAtOnce(t *testing.T) {
	fake := fakeDeps(fakeOptions{stalePanePID: true, deadPane: &deadPane{status: "3", signal: "", output: "launcher says no\n"}})
	assertDeepEqual(t, Run(context.Background(), startSeatArgs, fake.deps), 1)
	joined := strings.Join(fake.errors, " ")
	if !strings.Contains(joined, "exited with status 3") || !strings.Contains(joined, "launcher says no") {
		t.Fatalf("errors = %q, want the status and the pane output", fake.errors)
	}
	if !lastCallIsStop(fake.calls) {
		t.Fatalf("last call = %+v, want the unit stop", fake.calls[len(fake.calls)-1])
	}
}

// The race: the pane dies after the agent is found and before remain-on-exit is cleared.
func TestPaneDyingAfterTheAgentIsFoundStopsTheUnitAndReportsWhy(t *testing.T) {
	fake := fakeDeps(fakeOptions{diesAfterAgent: &deadPane{status: "2", signal: "", output: "Error: no module\n"}})
	assertDeepEqual(t, Run(context.Background(), startSeatArgs, fake.deps), 1)
	assertDeepEqual(t, fake.output, []string(nil))
	joined := strings.Join(fake.errors, " ")
	if !strings.Contains(joined, "exited with status 2") || !strings.Contains(joined, "Error: no module") {
		t.Fatalf("errors = %q, want the status and the pane output", fake.errors)
	}
	if !lastCallIsStop(fake.calls) {
		t.Fatalf("last call = %+v, want the unit stop", fake.calls[len(fake.calls)-1])
	}
}

// The report keeps the last 10 non-blank lines in order and drops tmux's pane-dead trailer.
func TestPaneReportKeepsLastTenNonBlankLinesAndDropsTheTrailer(t *testing.T) {
	var lines []string
	for i := 1; i <= 12; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	var output strings.Builder
	for _, line := range lines {
		output.WriteString(line + "\n\n")
	}
	output.WriteString("Pane is dead (status 3, Thu)\n")
	fake := fakeDeps(fakeOptions{deadPane: &deadPane{status: "3", signal: "", output: output.String()}})
	assertDeepEqual(t, Run(context.Background(), startSeatArgs, fake.deps), 1)
	want := "wave-seat-holder: launcher exited with status 3\n" + strings.Join(lines[2:], "\n") + "\n"
	assertDeepEqual(t, fake.errors, []string{want})
}
