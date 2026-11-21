package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/RigelBuild/nebula/tools/windows-agent-sync/internal/envstore"
	"github.com/samber/oops"
)

func filterMCP(raw []byte) ([]byte, []string, error) {
	var config map[string]json.RawMessage
	if err := json.Unmarshal(raw, &config); err != nil {
		return nil, nil, fmt.Errorf("decoding MCP config: %w", err)
	}
	serversData, ok := config["mcpServers"]
	if !ok {
		return nil, nil, errors.New("MCP config has no mcpServers object")
	}
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(serversData, &servers); err != nil {
		return nil, nil, fmt.Errorf("decoding MCP servers: %w", err)
	}
	kept := make(map[string]json.RawMessage, len(servers))
	dropped := make([]string, 0)
	for name, rawServer := range servers {
		var server struct {
			Type    string          `json:"type"`
			Command json.RawMessage `json:"command"`
		}
		if err := json.Unmarshal(rawServer, &server); err != nil {
			return nil, nil, fmt.Errorf("decoding MCP server %q: %w", name, err)
		}
		switch server.Type {
		case "http":
			kept[name] = rawServer
		case "stdio":
			dropped = append(dropped, name)
		case "":
			if len(server.Command) == 0 {
				return nil, nil, fmt.Errorf("MCP server %q has unsupported type %q", name, server.Type)
			}
			dropped = append(dropped, name)
		default:
			return nil, nil, fmt.Errorf("MCP server %q has unsupported type %q", name, server.Type)
		}
	}
	sort.Strings(dropped)
	encodedServers, err := json.Marshal(kept)
	if err != nil {
		return nil, nil, fmt.Errorf("encoding MCP servers: %w", err)
	}
	config["mcpServers"] = encodedServers
	out, err := json.MarshalIndent(config, "", "\t")
	if err != nil {
		return nil, nil, fmt.Errorf("encoding MCP config: %w", err)
	}
	return append(out, '\n'), dropped, nil
}

func syncMCP(co checkout, home string) error {
	source := co.path(co.layout.MCP)
	raw, err := os.ReadFile(source) //nolint:gosec // source is the MCP input the layout names under --repo-root.
	if err != nil {
		return oops.In("windows-agent-sync").Wrapf(err, "reading MCP source")
	}
	filtered, _, err := filterMCP(raw)
	if err != nil {
		return oops.In("windows-agent-sync").Wrapf(err, "filtering MCP source")
	}
	target := filepath.Join(home, ".omp", "agent", "mcp.json")
	current, err := os.ReadFile(target) //nolint:gosec // target is the generated MCP config under the current user's profile.
	if err == nil && bytes.Equal(current, filtered) {
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return oops.In("windows-agent-sync").Wrapf(err, "checking MCP target")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return oops.In("windows-agent-sync").Wrapf(err, "creating MCP target directory")
	}
	file, err := os.CreateTemp(filepath.Dir(target), ".mcp-*.tmp")
	if err != nil {
		return oops.In("windows-agent-sync").Wrapf(err, "staging MCP config")
	}
	temp := file.Name()
	_, writeErr := file.Write(filtered)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		removeErr := os.Remove(temp)
		return oops.In("windows-agent-sync").Wrapf(errors.Join(err, removeErr), "staging MCP config")
	}
	if err := envstore.ReplaceFile(temp, target); err != nil {
		removeErr := os.Remove(temp)
		return oops.In("windows-agent-sync").Wrapf(errors.Join(err, removeErr), "writing MCP config")
	}
	return nil
}
