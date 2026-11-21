// Package pane reads and manages the seat's tmux pane: it keeps a dead launcher's pane
// readable, reports why the launcher died, and clears remain-on-exit for a healthy agent.
package pane

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// SessionName is the tmux session every seat runs.
const SessionName = "seat"

// Result is the outcome of a tmux command that ran to completion.
type Result struct {
	Code   int
	Stdout string
	Stderr string
}

// Spawn runs tmux with args and returns its result. A failure to start is an error.
type Spawn func(ctx context.Context, tmux string, args []string) (Result, error)

// paneDeadTrailer matches tmux's trailer for a dead pane. It is not the launcher's output.
var paneDeadTrailer = regexp.MustCompile(`^Pane is dead \(`)

const (
	// deadPaneLines is how many non-blank pane lines a dead-launcher report keeps.
	deadPaneLines = 10
	// deadPaneCapture is the capture-pane start line: the last 20 lines, counted from the bottom.
	deadPaneCapture = "-20"
)

// Keep turns remain-on-exit on so a dead launcher's pane can still be read.
func Keep(ctx context.Context, spawn Spawn, tmux, socketPath string) error {
	return setRemainOnExit(ctx, spawn, tmux, socketPath, "on")
}

// Clear turns remain-on-exit off so a healthy agent's exit still closes the session.
// A pane that died first is returned as an error carrying the dead-pane report,
// not treated as healthy. It returns nil when the pane is alive.
func Clear(ctx context.Context, spawn Spawn, tmux, socketPath string) error {
	if err := setRemainOnExit(ctx, spawn, tmux, socketPath, "off"); err != nil {
		return err
	}
	dead, err := paneDead(ctx, spawn, tmux, socketPath)
	if err != nil {
		return err
	}
	if !dead {
		return nil
	}
	report, err := DeadReport(ctx, spawn, tmux, socketPath)
	if err != nil {
		return err
	}
	return errors.New(report)
}

// DeadReport names the launcher's exit cause and its last pane lines.
func DeadReport(ctx context.Context, spawn Spawn, tmux, socketPath string) (string, error) {
	status, signal, err := paneDeadStatus(ctx, spawn, tmux, socketPath)
	if err != nil {
		return "", err
	}
	output, err := capturePane(ctx, spawn, tmux, socketPath)
	if err != nil {
		return "", err
	}
	cause := "exited with status " + status
	if signal != "" {
		cause = "killed by signal " + signal
	}
	lines := append([]string{"launcher " + cause}, lastNonBlankPaneLines(output)...)
	return strings.Join(lines, "\n"), nil
}

// setRemainOnExit sets the global remain-on-exit option to value.
func setRemainOnExit(ctx context.Context, spawn Spawn, tmux, socketPath, value string) error {
	result, err := spawn(ctx, tmux, []string{"-N", "-S", socketPath, "set-option", "-g", "remain-on-exit", value})
	if err != nil {
		return fmt.Errorf("tmux set-option failed: %w", err)
	}
	if result.Code != 0 {
		return fmt.Errorf("tmux set-option failed: %s", Detail(result))
	}
	return nil
}

// paneDead reports whether tmux says the seat pane has exited.
func paneDead(ctx context.Context, spawn Spawn, tmux, socketPath string) (bool, error) {
	result, err := spawn(ctx, tmux, []string{"-N", "-S", socketPath, "display", "-p", "-t", SessionName, "#{pane_dead}"})
	if err != nil {
		return false, fmt.Errorf("tmux display failed: %w", err)
	}
	if result.Code != 0 {
		return false, fmt.Errorf("tmux display failed: %s", Detail(result))
	}
	return strings.TrimSpace(result.Stdout) == "1", nil
}

// paneDeadStatus returns the exit status and signal tmux recorded for the dead seat pane.
func paneDeadStatus(ctx context.Context, spawn Spawn, tmux, socketPath string) (string, string, error) {
	result, err := spawn(ctx, tmux, []string{"-N", "-S", socketPath, "display", "-p", "-t", SessionName, "#{pane_dead_status}|#{pane_dead_signal}"})
	if err != nil {
		return "", "", fmt.Errorf("tmux display failed: %w", err)
	}
	if result.Code != 0 {
		return "", "", errors.New("tmux display failed: " + Detail(result))
	}
	status, signal, _ := strings.Cut(strings.TrimSpace(result.Stdout), "|")
	if status == "" {
		status = "unknown"
	}
	return status, signal, nil
}

// capturePane returns the last deadPaneCapture lines of the seat pane.
func capturePane(ctx context.Context, spawn Spawn, tmux, socketPath string) (string, error) {
	result, err := spawn(ctx, tmux, []string{"-N", "-S", socketPath, "capture-pane", "-p", "-t", SessionName, "-S", deadPaneCapture})
	if err != nil {
		return "", fmt.Errorf("tmux capture-pane failed: %w", err)
	}
	return result.Stdout, nil
}

// lastNonBlankPaneLines keeps the last deadPaneLines non-blank lines, without tmux's pane-dead trailer.
func lastNonBlankPaneLines(output string) []string {
	var kept []string
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimRight(line, " \t\r")
		if line == "" || paneDeadTrailer.MatchString(line) {
			continue
		}
		kept = append(kept, line)
	}
	if len(kept) > deadPaneLines {
		kept = kept[len(kept)-deadPaneLines:]
	}
	return kept
}

// Detail is the trimmed stderr of a failed command, or its exit code when stderr is blank.
func Detail(result Result) string {
	if detail := strings.TrimSpace(result.Stderr); detail != "" {
		return detail
	}
	return fmt.Sprintf("exit %d", result.Code)
}
