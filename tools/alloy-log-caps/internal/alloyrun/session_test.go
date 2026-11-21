package alloyrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLabelPairsSortsAndAppendsStreamID(t *testing.T) {
	got := labelPairs(map[string]string{"b": "2", "a": "1"}, "s")
	want := `"a" = "1", "b" = "2", "stream_id" = "s"`
	if got != want {
		t.Errorf("labelPairs = %q, want %q", got, want)
	}
}

// stubAlloy writes a shell script that stands in for the Alloy binary.
func stubAlloy(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "alloy")
	// The stub has to run, so it needs the execute bit.
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return path
}

func oneStream() []Stream {
	return []Stream{{Name: "s", Labels: map[string]string{"k": "v"}, Lines: []string{"a", "b"}}}
}

func TestRunReportsEarlyAlloyExit(t *testing.T) {
	alloy := stubAlloy(t, "exit 3")
	_, err := run(context.Background(), "early", alloy, "", "", oneStream(), 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "Alloy exited 3") {
		t.Errorf("run error = %v, want an early-exit error", err)
	}
}

func TestRunReportsDrainDeadline(t *testing.T) {
	// exec replaces the shell, so SIGTERM from stop reaches sleep directly.
	alloy := stubAlloy(t, "exec sleep 30")
	_, err := run(context.Background(), "slow", alloy, "", "", oneStream(), 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not drain") {
		t.Errorf("run error = %v, want a drain-deadline error", err)
	}
}
