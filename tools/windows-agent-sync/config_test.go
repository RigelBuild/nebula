package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// validLayoutFields is the layout the rejection cases start from. Each case
// replaces one field, so a failure names the field that broke.
var validLayoutFields = map[string]string{
	"linkTable":  `"example/links.cue"`,
	"pins":       `"example/pins.json"`,
	"nebulaLock": `"example/flake.lock"`,
	"mcp":        `"example/agents/mcp.json"`,
	"secretMap":  `"secrets/env-map.nix"`,
}

// layoutSource renders a layout CUE file from fields, in a fixed order.
func layoutSource(fields map[string]string) string {
	order := []string{"linkTable", "pins", "nebulaLock", "mcp", "secretMap"}
	var b strings.Builder
	for _, name := range order {
		if value, ok := fields[name]; ok {
			b.WriteString(name + ": " + value + "\n")
		}
	}
	for name, value := range fields {
		if !slices.Contains(order, name) {
			b.WriteString(name + ": " + value + "\n")
		}
	}
	return b.String()
}

// withField returns the valid layout with one field replaced, or removed when value is empty.
func withField(name, value string) map[string]string {
	fields := make(map[string]string, len(validLayoutFields)+1)
	for key, val := range validLayoutFields {
		fields[key] = val
	}
	if value == "" {
		delete(fields, name)
	} else {
		fields[name] = value
	}
	return fields
}

func TestLoadLayoutAcceptsValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "layout.cue")
	if err := os.WriteFile(path, []byte(layoutSource(validLayoutFields)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadLayout(path); err != nil {
		t.Fatalf("loadLayout() rejected a valid layout: %v", err)
	}
}

func TestLoadLayoutRejectsInvalid(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
	}{
		{"parent escape", layoutSource(withField("pins", `"../x"`))},
		{"absolute path", layoutSource(withField("mcp", `"/abs"`))},
		{"nested parent escape", layoutSource(withField("secretMap", `"a/../../x"`))},
		{"empty value", layoutSource(withField("linkTable", `""`))},
		{"unknown field", layoutSource(withField("extra", `"x"`))},
		{"missing field", layoutSource(withField("nebulaLock", ""))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "layout.cue")
			if err := os.WriteFile(path, []byte(tc.source), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadLayout(path); err == nil {
				t.Fatalf("loadLayout() accepted %q", tc.source)
			}
		})
	}
}
