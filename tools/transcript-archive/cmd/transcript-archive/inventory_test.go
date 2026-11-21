package main

import (
	"os"
	"path/filepath"
	"testing"
)

// fixtureConfig is the synthetic archive config under testdata/.
const fixtureConfig = "../../testdata/archive.cue"

func TestInventoryEmptyHostWritesEmptyInventory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	out := filepath.Join(t.TempDir(), "inventory.jsonl")
	if err := runInventory("example-empty", fixtureConfig, "", "", out); err != nil {
		t.Fatalf("empty host failed: %v", err)
	}
	got, err := readInventory(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Lines) != 0 || len(got.Errors) != 0 {
		t.Fatalf("empty host inventory = %+v, want no lines or errors", got)
	}
}

func TestInventoryNonEmptyHostWithNoSourcesFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	configPath := filepath.Join(t.TempDir(), "archive.cue")
	if err := os.WriteFile(configPath, []byte(`package archive
hosts: bare: {
	state: "unmeasured"
	sources: []
	watch: []
	exclude: []
	finalOnly: []
}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "inventory.jsonl")
	if err := runInventory("bare", configPath, "", "", out); err == nil {
		t.Fatal("unmeasured host with no sources accepted")
	}
}

func TestInventoryRequiresConfig(t *testing.T) {
	t.Parallel()
	command := newInventoryCmd()
	command.SilenceUsage = true
	command.SilenceErrors = true
	command.SetArgs([]string{"--host", "example-host", "--out", filepath.Join(t.TempDir(), "inventory.jsonl")})
	if err := command.ExecuteContext(t.Context()); err == nil {
		t.Fatal("inventory without --config exited zero")
	}
}
