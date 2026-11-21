package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/inventory"
)

func TestCoverageCommandHistoryAndAssignment(t *testing.T) {
	dir := t.TempDir()
	digest := sha256.Sum256([]byte("old"))
	line := inventory.Line{Path: "/old", Type: "file", Class: "normal", Size: 3, SHA256: digest}
	row, err := json.Marshal(inventory.DeltaRow{Event: "added", Line: &line})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"final": "{\"outsideTargets\":0}\n", "baseline": "{\"outsideTargets\":0}\n",
		"delta-1.jsonl": string(row) + "\n", "journal": "{\"event\":\"pass-stored\",\"passId\":\"1\"}\n",
		"digests": "{}", "vanished": "", "password": "secret",
		"restic": "#!/bin/sh\ncase \" $* \" in\n*' snapshots '*) echo '[{\"id\":\"old\",\"time\":\"2026-01-01T00:00:00Z\",\"tags\":[\"pass-1\"]}]';;\n*' old '*) echo '{\"struct_type\":\"node\",\"type\":\"file\",\"path\":\"/old\"}';;\n*) echo '{\"struct_type\":\"snapshot\"}';;\nesac\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	args := []string{"--final", filepath.Join(dir, "final"), "--baseline", filepath.Join(dir, "baseline"), "--deltas", filepath.Join(dir, "delta-1.jsonl"), "--scope", "test", "--snapshot", "final", "--repo", dir, "--password-file", filepath.Join(dir, "password"), "--journal", filepath.Join(dir, "journal"), "--digests", filepath.Join(dir, "digests"), "--vanished", filepath.Join(dir, "vanished")}
	command := newCoverageCmd()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs(args)
	if err := command.Execute(); err != nil {
		t.Fatalf("history should explain missing version: %v: %s", err, output.String())
	}
	if !strings.Contains(output.String(), `"snapshotId":"old"`) {
		t.Fatalf("history missing: %s", output.String())
	}
	if err := os.WriteFile(filepath.Join(dir, "final"), []byte("{\"error\":{\"path\":\"/unknown\",\"reason\":\"unknown\"}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command = newCoverageCmd()
	command.SetOut(&output)
	command.SetArgs(args)
	if err := command.Execute(); err == nil {
		t.Fatal("assignment error should fail coverage")
	}
}
