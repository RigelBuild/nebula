package issueowner

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"
)

func TestRosterSetStatus(t *testing.T) {
	tests := []struct {
		name     string
		set      RosterSet
		token    string
		expected RosterStatus
	}{
		{
			name: "rostered takes precedence over live",
			set: RosterSet{
				Rostered: map[string]struct{}{"ci-build": {}},
				Live:     map[string]struct{}{"ci-build": {}},
			},
			token:    "ci-build",
			expected: Rostered,
		},
		{
			name: "live but unrostered",
			set: RosterSet{
				Rostered: map[string]struct{}{},
				Live:     map[string]struct{}{"mercator": {}},
			},
			token:    "mercator",
			expected: LiveUnrostered,
		},
		{
			name:     "unknown",
			set:      RosterSet{},
			token:    "",
			expected: Unknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.set.Status(tt.token); got != tt.expected {
				t.Fatalf("Status(%q) = %q, want %q", tt.token, got, tt.expected)
			}
		})
	}
}

func TestLoadRosterSet(t *testing.T) {
	now := time.Date(2026, 9, 23, 18, 40, 24, 875_000_000, time.UTC)
	tests := []struct {
		name              string
		rosterMissing     bool
		workspacesMissing bool
		wantRostered      []string
		wantLive          []string
		wantWarning       func(rosterPath, workspacesRoot string) string
	}{
		{name: "loads roster and directory names", wantRostered: []string{"fixture"}, wantLive: []string{"active"}},
		{
			name: "reports unavailable roster but retains live names", rosterMissing: true,
			wantRostered: []string{}, wantLive: []string{"active"},
			wantWarning: func(rosterPath, _ string) string {
				return fmt.Sprintf("canonical roster unavailable (read roster %q: open %s: no such file or directory)", rosterPath, rosterPath)
			},
		},
		{
			name: "reports unavailable workspace root but retains roster", workspacesMissing: true,
			wantRostered: []string{"fixture"}, wantLive: []string{},
			wantWarning: func(_, workspacesRoot string) string {
				return fmt.Sprintf("workspace root %s could not be listed (open %s: no such file or directory)", workspacesRoot, workspacesRoot)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rosterPath, workspacesRoot := writeRosterSetFixture(t, !tt.rosterMissing, !tt.workspacesMissing)
			wantWarnings := []string{}
			if tt.wantWarning != nil {
				wantWarnings = []string{tt.wantWarning(rosterPath, workspacesRoot)}
			}
			got := LoadRosterSet(t.Context(), rosterPath, workspacesRoot, now)
			if got.Rostered == nil || got.Live == nil || got.Warnings == nil {
				t.Fatal("LoadRosterSet returned nil collections")
			}
			if names := sortedSetNames(got.Rostered); !reflect.DeepEqual(names, tt.wantRostered) {
				t.Errorf("Rostered = %v, want %v", names, tt.wantRostered)
			}
			if names := sortedSetNames(got.Live); !reflect.DeepEqual(names, tt.wantLive) {
				t.Errorf("Live = %v, want %v", names, tt.wantLive)
			}
			if !reflect.DeepEqual(got.Warnings, wantWarnings) {
				t.Errorf("Warnings = %v, want %v", got.Warnings, wantWarnings)
			}
		})
	}
}

// writeRosterSetFixture returns roster and workspace paths, creating only the ones requested.
func writeRosterSetFixture(t *testing.T, withRoster, withWorkspaces bool) (string, string) {
	t.Helper()
	root := t.TempDir()
	rosterPath := filepath.Join(root, "missing.json")
	workspacesRoot := filepath.Join(root, "missing-workspaces")
	if withRoster {
		rosterPath = filepath.Join(root, "roster.json")
		content := `{"schemaVersion":"roster-v1","sourceRevision":"rev","generatedAt":"2026-09-23T18:40:24.875Z","provenance":"test","agents":[{"name":"fixture"}]}`
		if err := os.WriteFile(rosterPath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if withWorkspaces {
		workspacesRoot = filepath.Join(root, "workspaces")
		if err := os.MkdirAll(filepath.Join(workspacesRoot, "active"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(workspacesRoot, "not-a-dir"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return rosterPath, workspacesRoot
}

func TestLoadRosterSetDefaults(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "agents", "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "agents", "workspaces", "agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	content := `{"schemaVersion":"roster-v1","sourceRevision":"rev","generatedAt":"2026-09-23T18:40:24.875Z","provenance":"test","agents":[{"name":"fixture"}]}`
	rosterPath := filepath.Join(root, "agents", "state", "roster.json")
	if err := os.WriteFile(rosterPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	t.Setenv("ROSTER_PATH", "")
	set := LoadRosterSet(t.Context(), "", "", time.Date(2026, 9, 23, 18, 40, 24, 875_000_000, time.UTC))
	if set.Status("fixture") != Rostered || set.Status("agent") != LiveUnrostered {
		t.Fatalf("default paths were not used: %+v", set)
	}
}

func sortedSetNames(values map[string]struct{}) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
