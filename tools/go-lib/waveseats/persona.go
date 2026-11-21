// SPDX-License-Identifier: MIT OR Apache-2.0

package waveseats

import (
	"errors"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Persona holds the fields used to place a seat.
type Persona struct {
	Name string
	Role *string
	Tab  *string
}

var personaFrontmatter = regexp.MustCompile(`(?s)^---\r?\n(.*?)\r?\n---(?:\r?\n|$)`)

// ParsePersona reads the YAML frontmatter of a persona.
func ParsePersona(source string) (Persona, error) {
	match := personaFrontmatter.FindStringSubmatch(source)
	if match == nil {
		return Persona{}, errors.New("persona has no YAML frontmatter")
	}
	var value any
	if err := yaml.Unmarshal([]byte(match[1]), &value); err != nil {
		return Persona{}, err
	}
	fields, ok := value.(map[string]any)
	if !ok {
		return Persona{}, errors.New("persona frontmatter must be a mapping")
	}
	name, ok := fields["name"].(string)
	if !ok || name == "" {
		return Persona{}, errors.New("persona frontmatter must have a name")
	}
	persona := Persona{Name: name}
	if role, ok := fields["role"].(string); ok {
		persona.Role = &role
	}
	if zellij, ok := fields["zellij"].(map[string]any); ok {
		if tab, ok := zellij["tab"].(string); ok && strings.TrimSpace(tab) != "" {
			persona.Tab = &tab
		}
	}
	return persona, nil
}
