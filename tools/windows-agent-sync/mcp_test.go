package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSyncMCPReplacesExistingTarget(t *testing.T) {
	co := checkout{root: t.TempDir(), layout: layout{MCP: "example/agents/mcp.json"}}
	home := t.TempDir()
	source := co.path(co.layout.MCP)
	if err := os.MkdirAll(filepath.Dir(source), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte(`{"mcpServers":{"http":{"type":"http","url":"https://example.test"},"flux-example":{"command":"example-mcp","args":["serve","--transport","stdio"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, ".omp", "agent", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("old config"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syncMCP(co, home); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(got, []byte("old config")) || !bytes.Contains(got, []byte("https://example.test")) || bytes.Contains(got, []byte("flux-example")) {
		t.Fatalf("MCP target = %q, want HTTP server retained and implicit stdio server dropped", got)
	}
}

func TestFilterMCP(t *testing.T) {
	input := []byte(`{"mcpServers":{"http":{"type":"http","url":"https://example.test"},"stdio":{"type":"stdio","command":"server"},"implicit-stdio":{"command":"server"}}}`)
	output, dropped, err := filterMCP(input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(dropped, []string{"implicit-stdio", "stdio"}) {
		t.Fatalf("dropped = %v", dropped)
	}
	var got struct {
		Servers map[string]struct {
			Type string `json:"type"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(output, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Servers) != 1 || got.Servers["http"].Type != "http" {
		t.Fatalf("servers = %+v", got.Servers)
	}
}
