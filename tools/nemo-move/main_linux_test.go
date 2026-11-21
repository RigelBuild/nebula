//go:build linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProbeStatus(t *testing.T) {
	now := time.Now().UTC()
	processStart, err := readProcessStartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		kind      string
		phase     string
		status    string
		startTime int64
		wantError bool
	}{
		{name: "startup passes while blocked", kind: "startup", phase: "blocked"},
		{name: "live passes while blocked", kind: "live", phase: "blocked"},
		{name: "ready fails while blocked", kind: "ready", phase: "blocked", wantError: true},
		{name: "missing phase passes startup", kind: "startup"},
		{name: "ready fails when status missing", kind: "ready", phase: "owner", wantError: true},
		{name: "unhealthy gateway fails live", kind: "live", phase: "owner", status: "startup_failed", startTime: processStart, wantError: true},
		{name: "healthy gateway passes ready", kind: "ready", phase: "owner", status: "running", startTime: processStart},
		{name: "recycled process fails live", kind: "live", phase: "owner", status: "running", startTime: processStart + 1, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			phasePath := filepath.Join(root, "phase")
			if test.phase != "" {
				if err := os.WriteFile(phasePath, []byte(test.phase), 0600); err != nil {
					t.Fatal(err)
				}
			}
			statusPath := filepath.Join(root, "gateway_state.json")
			if test.status != "" {
				data, err := json.Marshal(map[string]any{
					"pid":           os.Getpid(),
					"start_time":    test.startTime,
					"updated_at":    now.Format(time.RFC3339Nano),
					"gateway_state": test.status,
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(statusPath, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := probeStatus(test.kind, phasePath, statusPath, now)
			if (err != nil) != test.wantError {
				t.Fatalf("probeStatus() error = %v, wantError %t", err, test.wantError)
			}
		})
	}
}

func TestProbeStatusRequiresRecentStatus(t *testing.T) {
	processStart, err := readProcessStartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	root := t.TempDir()
	phasePath := filepath.Join(root, "phase")
	if err := os.WriteFile(phasePath, []byte("owner"), 0600); err != nil {
		t.Fatal(err)
	}
	statusPath := filepath.Join(root, "gateway_state.json")
	data, err := json.Marshal(map[string]any{
		"pid":           os.Getpid(),
		"start_time":    processStart,
		"updated_at":    now.Add(-statusStaleAfter - time.Second).Format(time.RFC3339Nano),
		"gateway_state": "running",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statusPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := probeStatus("ready", phasePath, statusPath, now); err == nil {
		t.Fatal("probeStatus() accepted stale gateway status")
	}
}
