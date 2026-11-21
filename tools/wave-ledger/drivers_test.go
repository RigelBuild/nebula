package main

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestDrivers(t *testing.T) {
	agent := "agent-a"
	input := int64(100)
	tool := "read"
	rows := []Row{{Kind: kindModelUsage, SessionID: "s", Agent: &agent, Input: &input}}
	content := []ContentRow{
		{SessionID: "s", Role: "user", Parts: []json.RawMessage{json.RawMessage(`{"type":"text","text":"private"}`)}},
		{SessionID: "s", Role: "toolResult", ToolName: &tool, Parts: []json.RawMessage{json.RawMessage(`{"type":"text","text":"1234567"}`)}},
		{SessionID: "other", Role: "user", Parts: []json.RawMessage{json.RawMessage(`{"type":"text","text":"ignore"}`)}},
	}
	got, _, _, err := Drivers(rows, t.TempDir(), func(fn func(ContentRow) error) error {
		for _, row := range content {
			if err := fn(row); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("attribution", func(t *testing.T) {
		if len(got) != 2 || got[0].Key.Source != "tool_result:read" || got[0].Bytes != 7 || got[1].Share != 0.5 || got[1].EstInputTokens != 50 {
			t.Fatalf("unexpected: %+v", got)
		}
	})
	t.Run("agent_join", func(t *testing.T) {
		for _, d := range got {
			if d.Key.Agent != agent {
				t.Fatalf("unexpected agent: %+v", d)
			}
		}
	})
	t.Run("no_text_in_output", func(t *testing.T) {
		for _, asJSON := range []bool{false, true} {
			var out bytes.Buffer
			if err := writeDrivers(&out, driversJSON{Drivers: got}, asJSON); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "private") {
				t.Fatal("output contains private text")
			}
		}
	})
}

func TestPromptAttribution(t *testing.T) {
	dir := t.TempDir()
	promptDir := filepath.Join(dir, "content", "prompts")
	if err := os.MkdirAll(promptDir, 0700); err != nil {
		t.Fatal(err)
	}
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	if err := os.WriteFile(filepath.Join(promptDir, "abc.txt.zst"), enc.EncodeAll([]byte("hello"), nil), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(promptDir, "aaa.txt.zst"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	agent, digest, missing, invalid, corrupt := "agent", "abc", "def", "private/path", "aaa"
	rows := []Row{{Kind: kindMessage, Agent: &agent, PromptDigest: &digest}, {Kind: kindModelUsage, Agent: &agent, PromptDigest: &digest}, {Kind: kindModelUsage, Agent: &agent, PromptDigest: &missing}, {Kind: kindMessage, Agent: &agent}, {Kind: kindMessage, Agent: &agent, PromptDigest: &invalid}, {Kind: kindMessage, Agent: &agent, PromptDigest: &corrupt}}
	d := newDriverTotals(rows)
	counts := d.addPrompts(dir, rows)
	if counts.NoDigest != 1 || counts.BadDigest != 1 || counts.BadPromptFile != 1 || counts.MissingPromptFile != 1 || len(counts.BadDigestShapes) != 1 || counts.BadDigestShapes[0].Class != "nonhex" || d.systems[agent].Bytes != 10 || d.systems[agent].Rows != 2 {
		t.Fatalf("unexpected prompt attribution: %+v %+v", counts, d.systems)
	}
}

func TestPromptArchiveDigest(t *testing.T) {
	dir := t.TempDir()
	promptDir := filepath.Join(dir, "content", "prompts")
	if err := os.MkdirAll(promptDir, 0700); err != nil {
		t.Fatal(err)
	}
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	if err := os.WriteFile(filepath.Join(promptDir, "abc.txt.zst"), enc.EncodeAll([]byte("archive"), nil), 0600); err != nil {
		t.Fatal(err)
	}
	agent, group, archive := "agent", "def", "abc"
	rows := []Row{{Kind: kindMessage, Agent: &agent, PromptDigest: &group, PromptArchiveDigest: &archive}}
	d := newDriverTotals(rows)
	counts := d.addPrompts(dir, rows)
	if counts.MissingPromptFile != 0 || d.systems[agent].Bytes != 7 {
		t.Fatalf("unexpected attribution: %+v %+v", counts, d.systems)
	}
}

func TestDriverSharesAndMessageJoin(t *testing.T) {
	first, second := "first", "second"
	read, bash := "read", "bash"
	input := int64(120)
	rows := []Row{{Kind: kindMessage, Agent: &first, SessionID: "one", Input: &input}, {Kind: kindModelUsage, Agent: &second, SessionID: "two", Input: &input}}
	d := newDriverTotals(rows)
	d.systems[first] = SystemDriver{Agent: first, Bytes: 4, Rows: 1}
	for _, row := range []ContentRow{
		{SessionID: "one", Role: "toolResult", ToolName: &read, Parts: []json.RawMessage{json.RawMessage(`{"type":"text","text":"1234"}`)}},
		{SessionID: "one", Role: "toolResult", ToolName: &bash, Parts: []json.RawMessage{json.RawMessage(`{"type":"text","text":"5678"}`)}},
		{SessionID: "two", Role: "user", Parts: []json.RawMessage{json.RawMessage(`{"type":"text","text":"xx"}`)}},
	} {
		if err := d.addContent(row); err != nil {
			t.Fatal(err)
		}
	}
	drivers := d.result()
	systems := d.systemResults()
	shares := map[string]float64{}
	for _, driver := range drivers {
		shares[driver.Key.Agent] += driver.Share
	}
	for _, system := range systems {
		shares[system.Agent] += system.Share
	}
	if math.Abs(shares[first]-1) > 1e-9 || math.Abs(shares[second]-1) > 1e-9 {
		t.Fatalf("shares: %v", shares)
	}
	var firstAgentTokens int64
	for _, driver := range drivers {
		if driver.Key.Agent == first {
			firstAgentTokens += driver.EstInputTokens
			if math.Abs(driver.Share-(1.0/3.0)) > 1e-9 {
				t.Fatalf("three-way driver share = %.12f, want 1/3", driver.Share)
			}
		}
		if math.Abs(driver.Share*float64(input)-float64(driver.EstInputTokens)) > 1e-9 {
			t.Fatalf("driver share and input estimate differ: %+v", driver)
		}
	}
	if len(systems) != 1 || math.Abs(systems[0].Share-(1.0/3.0)) > 1e-9 || math.Abs(systems[0].Share*float64(input)-float64(systems[0].EstInputTokens)) > 1e-9 {
		t.Fatalf("system share and input estimate differ: %+v", systems)
	}
	if d.bytes[DriverKey{Agent: first, Source: "tool_result:read"}] != 4 || d.bytes[DriverKey{Agent: first, Source: "tool_result:bash"}] != 4 || firstAgentTokens+systems[0].EstInputTokens != input {
		t.Fatalf("unexpected attribution: drivers=%+v systems=%+v", drivers, systems)
	}
}

func TestDriverCoverage(t *testing.T) {
	a, b, invalid, digest := "a", "b", "bad agent", "abc"
	input := int64(100)
	rows := []Row{
		{Kind: kindMessage, Agent: &a, SessionID: "one", Input: &input},
		{Kind: kindMessage, Agent: &b, SessionID: "one", Input: &input, PromptDigest: &digest},
		{Kind: kindModelUsage, Agent: &b, SessionID: "one", Input: &input},
		{Kind: kindMessage, Agent: &a, SessionID: "two", Input: &input},
		{Kind: kindMessage, Agent: &b, SessionID: "two", Input: &input, PromptDigest: &digest},
		{Kind: kindMessage, Agent: &invalid},
	}
	_, _, counts, err := Drivers(rows, t.TempDir(), func(func(ContentRow) error) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if counts.InvalidAgentRows != 1 || counts.ConflictingSessions != 2 || counts.ConflictingRows != 3 || counts.MissingPromptFile != 0 || len(counts.Unattributed) != 1 || counts.Unattributed[0].Agent != a {
		t.Fatalf("coverage: %+v", counts)
	}
}

func TestRunDrivers(t *testing.T) {
	root := t.TempDir()
	date := "2026-11-09"
	row := `{"v":1,"kind":"message","ts":"2026-11-09T00:00:00Z","row_key":"one","session_id":"s","agent":"agent","input":10}` + "\n"
	if err := os.WriteFile(filepath.Join(root, date+".jsonl"), []byte(row), 0600); err != nil {
		t.Fatal(err)
	}
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	badFrame := enc.EncodeAll([]byte("{invalid}\n"), nil)
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "content"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "content", date+".jsonl.zst"), badFrame, 0600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		args       []string
		wantErr    string
		wantOutput string
	}{
		{name: "to_before_from", args: []string{"--to", "2026-11-08", "--from", date, "--dir", root}, wantErr: "--to must be on or after --from"},
		{name: "bad_date", args: []string{"--to", date, "--from", "bad-date", "--dir", root}, wantErr: "invalid --from"},
		{name: "bad_frames_output", args: []string{"--to", date, "--from", date, "--dir", root}, wantOutput: "bad_frames: 1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := runDrivers(tc.args, &stdout, &stderr)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(stdout.String(), tc.wantOutput) {
				t.Fatalf("output = %q, want %q", stdout.String(), tc.wantOutput)
			}
		})
	}
}
