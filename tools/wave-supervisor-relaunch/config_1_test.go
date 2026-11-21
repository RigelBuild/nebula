package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTakeConfigFlagTable(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantPath string
		wantRest []string
		wantErr  string
	}{
		{name: "missing flag", args: []string{"run"}, wantErr: "--config <path> is required"},
		{name: "dangling flag at end", args: []string{"run", "--config"}, wantErr: "--config <path> is required once"},
		{name: "repeated flag", args: []string{"--config", "a.cue", "--config", "b.cue", "run"}, wantErr: "--config <path> is required once"},
		{name: "empty path", args: []string{"--config", "", "run"}, wantErr: "--config <path> is required once"},
		{name: "blank path", args: []string{"--config", "   ", "run"}, wantErr: "--config <path> is required once"},
		{name: "flag then rest keeps order", args: []string{"--config", "relaunch.cue", "stop-post", "--json"}, wantPath: "relaunch.cue", wantRest: []string{"stop-post", "--json"}},
		{name: "flag after subcommand", args: []string{"run", "--config", "relaunch.cue"}, wantPath: "relaunch.cue", wantRest: []string{"run"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, rest, err := takeConfigFlag(tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("takeConfigFlag(%q) error = %v, want %q", tc.args, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("takeConfigFlag(%q) error = %v", tc.args, err)
			}
			if path != tc.wantPath || !reflect.DeepEqual(rest, tc.wantRest) {
				t.Fatalf("takeConfigFlag(%q) = (%q, %q), want (%q, %q)", tc.args, path, rest, tc.wantPath, tc.wantRest)
			}
		})
	}
}

// fixtureConfigText is a complete, valid config. Each loadConfig case edits one line of it.
var fixtureConfigText = strings.Join([]string{
	`rootSeat: "foreman"`,
	`cotalSpace: "main"`,
	`endpointKind: "agent"`,
	`zellijSession: "viewer"`,
	`zellijUnit: "zellij-server.service"`,
	`cotalManagerUnit: "cotal-manager.service"`,
	`relaunchUnit: "wave-supervisor-relaunch.service"`,
	`waveSlice: "wave-agents.slice"`,
	`seatsPath: "/state/wave-seats.json"`,
	`stateDir: "/state"`,
	`homeDir: "/home/fixture"`,
	`sessionsDir: "/sessions"`,
	`hosts: ["seat-host-a"]`,
	`managerLogPath: "~/.cotal/manager.log"`,
	`journalUnits: ["cotal-manager", "zellij-server", "wave-supervisor-relaunch"]`,
	`relaunchIssue: "RIG-0001"`,
	`knownCause: "zellij server out of memory"`,
	`causeOwner: "owner-fixture"`,
	`seatCLI: "wave-seats"`,
	`spawnCommand: "spawn-agent"`,
	`seatHolderCLI: "wave-seat-holder"`,
	``,
}, "\n")

func writeConfigFile(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "relaunch.cue")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigTable(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		wantErr string
	}{
		{name: "valid config loads", text: fixtureConfigText},
		{name: "unknown field is refused", text: fixtureConfigText + "bogusField: \"x\"\n", wantErr: "bogusField"},
		{name: "missing field is refused", text: strings.Replace(fixtureConfigText, `seatCLI: "wave-seats"`+"\n", "", 1), wantErr: "validate config"},
		{name: "empty string value is refused", text: strings.Replace(fixtureConfigText, `rootSeat: "foreman"`, `rootSeat: ""`, 1), wantErr: "validate config"},
		{name: "empty hosts list is accepted", text: strings.Replace(fixtureConfigText, `hosts: ["seat-host-a"]`, `hosts: []`, 1)},
		{name: "empty journal units is refused", text: strings.Replace(fixtureConfigText, `journalUnits: ["cotal-manager", "zellij-server", "wave-supervisor-relaunch"]`, `journalUnits: []`, 1), wantErr: "validate config"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadConfig(writeConfigFile(t, tc.text))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("loadConfig() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("loadConfig() error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadConfigMissingFileIsRefused(t *testing.T) {
	if _, err := loadConfig(filepath.Join(t.TempDir(), "absent.cue")); err == nil || !strings.Contains(err.Error(), "read config") {
		t.Fatalf("loadConfig(absent) error = %v, want read config failure", err)
	}
}
