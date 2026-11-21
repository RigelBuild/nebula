package pane

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// LastNonBlankPaneLines drops blanks and tmux's trailer, keeping the lines in order.
func TestLastNonBlankPaneLinesDropsBlanksAndTrailer(t *testing.T) {
	got := lastNonBlankPaneLines("a\n\n  \nPane is dead (status 1)\nb\r\n")
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("lastNonBlankPaneLines = %q, want [a b]", got)
	}
}

// Only the last ten non-blank lines are kept, counted after blanks and the trailer are dropped.
func TestLastNonBlankPaneLinesKeepsTheLastTen(t *testing.T) {
	lines := []string{"line a", "line b", "line c", "line d", "line e", "line f", "line g", "line h", "line i", "line j", "line k", "line l"}
	var output strings.Builder
	for _, line := range lines {
		output.WriteString(line + "\n\n")
	}
	got := lastNonBlankPaneLines(output.String())
	if len(got) != deadPaneLines {
		t.Fatalf("kept %d lines, want %d", len(got), deadPaneLines)
	}
	if got[0] != "line c" || got[len(got)-1] != "line l" {
		t.Fatalf("kept %q, want lines c through l", got)
	}
}

// Detail prefers trimmed stderr and falls back to the exit code when stderr is blank.
func TestDetailPrefersStderrThenExitCode(t *testing.T) {
	if got := Detail(Result{Code: 2, Stderr: "  no server  \n"}); got != "no server" {
		t.Fatalf("Detail with stderr = %q, want %q", got, "no server")
	}
	if got := Detail(Result{Code: 2, Stderr: " \n"}); got != "exit 2" {
		t.Fatalf("Detail with blank stderr = %q, want %q", got, "exit 2")
	}
}

// Keep sets remain-on-exit on, against the seat socket.
func TestKeepTurnsRemainOnExitOn(t *testing.T) {
	var got []string
	spawn := func(_ context.Context, _ string, args []string) (Result, error) {
		got = args
		return Result{}, nil
	}
	if err := Keep(context.Background(), spawn, "tmux", "/run/s.sock"); err != nil {
		t.Fatalf("Keep: %v", err)
	}
	want := []string{"-N", "-S", "/run/s.sock", "set-option", "-g", "remain-on-exit", "on"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Keep args = %q, want %q", got, want)
	}
}

// Clear returns the dead-pane report when the launcher has already exited.
func TestClearReturnsTheDeadReportWhenPaneIsDead(t *testing.T) {
	spawn := func(_ context.Context, _ string, args []string) (Result, error) {
		switch {
		case slices.Contains(args, "#{pane_dead}"):
			return Result{Stdout: "1\n"}, nil
		case slices.Contains(args, "#{pane_dead_status}|#{pane_dead_signal}"):
			return Result{Stdout: "3|\n"}, nil
		case slices.Contains(args, "capture-pane"):
			return Result{Stdout: "boom\n"}, nil
		default:
			return Result{}, nil
		}
	}
	err := Clear(context.Background(), spawn, "tmux", "/run/s.sock")
	if err == nil {
		t.Fatal("Clear of a dead pane returned nil, want the report")
	}
	want := "launcher exited with status 3\nboom"
	if err.Error() != want {
		t.Fatalf("Clear error = %q, want %q", err.Error(), want)
	}
}

// Clear returns nil when the pane is alive.
func TestClearReturnsNilWhenPaneIsAlive(t *testing.T) {
	spawn := func(_ context.Context, _ string, args []string) (Result, error) {
		if slices.Contains(args, "#{pane_dead}") {
			return Result{Stdout: "0\n"}, nil
		}
		return Result{}, nil
	}
	if err := Clear(context.Background(), spawn, "tmux", "/run/s.sock"); err != nil {
		t.Fatalf("Clear of a live pane = %v, want nil", err)
	}
}
