// SPDX-License-Identifier: MIT OR Apache-2.0

package bundle

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/pelletier/go-toml/v2"
)

func ReadPublishConfig(root string, mode ConfigMode) (PublishConfig, error) {
	path := filepath.Join(root, "docs-bundle.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && mode == ConfigDefault {
			return PublishConfig{Exclude: []string{}, Sections: []SectionRule{}}, nil
		}
		if errors.Is(err, os.ErrNotExist) {
			return PublishConfig{}, fmt.Errorf("required publish config is missing: %s", path)
		}
		return PublishConfig{}, err
	}
	var input map[string]any
	if err := toml.Unmarshal(data, &input); err != nil {
		return PublishConfig{}, fmt.Errorf("invalid docs-bundle.toml: %s", err)
	}
	if input == nil {
		return PublishConfig{}, errors.New("docs-bundle.toml must be a table")
	}
	excludeValue, exists := input["exclude"]
	if !exists {
		return PublishConfig{}, errors.New("docs-bundle.toml requires exclude")
	}
	exclude, ok := stringSlice(excludeValue)
	if !ok {
		return PublishConfig{}, errors.New("exclude must be an array of strings")
	}
	config := PublishConfig{Exclude: exclude, Sections: []SectionRule{}}
	if value, exists := input["sections"]; exists && value != nil {
		sections, ok := value.([]any)
		if !ok {
			return PublishConfig{}, errors.New("sections must be an array of tables")
		}
		config.Sections = make([]SectionRule, 0, len(sections))
		for index, value := range sections {
			section, err := parseSection(value, index)
			if err != nil {
				return PublishConfig{}, err
			}
			config.Sections = append(config.Sections, section)
		}
	}
	if value, exists := input["packages"]; exists {
		config.Packages, err = parsePackages(value)
		if err != nil {
			return PublishConfig{}, err
		}
	}
	return config, nil
}

func parseSection(value any, index int) (SectionRule, error) {
	input, ok := value.(map[string]any)
	if !ok {
		return SectionRule{}, fmt.Errorf("sections[%d] must be a table", index)
	}
	id, idOK := input["id"].(string)
	label, labelOK := input["label"].(string)
	route, routeOK := input["route"].(string)
	if !idOK || !labelOK || !routeOK {
		return SectionRule{}, fmt.Errorf("sections[%d] requires string id, label, and route", index)
	}
	section := SectionRule{ID: id, Label: label, Route: route}
	if value, exists := input["prefix"]; exists {
		prefix, ok := value.(string)
		if !ok {
			return SectionRule{}, fmt.Errorf("sections[%d].prefix must be a string", index)
		}
		section.Prefix = new(prefix)
	}
	if value, exists := input["files"]; exists {
		files, ok := stringSlice(value)
		if !ok {
			return SectionRule{}, fmt.Errorf("sections[%d].files must be an array of strings", index)
		}
		section.Files = files
	}
	if value, exists := input["pagefind"]; exists {
		pagefind, ok := value.(bool)
		if !ok {
			return SectionRule{}, fmt.Errorf("sections[%d].pagefind must be a boolean", index)
		}
		section.Pagefind = new(pagefind)
	}
	if value, exists := input["collapsed"]; exists {
		collapsed, ok := value.(bool)
		if !ok {
			return SectionRule{}, fmt.Errorf("sections[%d].collapsed must be a boolean", index)
		}
		section.Collapsed = new(collapsed)
	}
	if section.Prefix == nil && section.Files == nil {
		return SectionRule{}, fmt.Errorf("sections[%d] requires prefix or files", index)
	}
	return section, nil
}

func parsePackages(value any) (*PackageRules, error) {
	input, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("packages must be a table")
	}
	route, ok := input["route"].(string)
	if !ok {
		return nil, errors.New("packages.route must be a string")
	}
	allow, ok := stringSlice(input["allow"])
	if !ok {
		return nil, errors.New("packages.allow must be an array of strings")
	}
	depth, ok := input["readme_depth"].(int64)
	if !ok || depth < 0 || int64(int(depth)) != depth {
		return nil, errors.New("packages.readme_depth must be a non-negative integer")
	}
	packages := &PackageRules{Route: route, Allow: allow, ReadmeDepth: int(depth)}
	if value, exists := input["nested"]; exists {
		packages.Nested, ok = stringSlice(value)
		if !ok {
			return nil, errors.New("packages.nested must be an array of strings")
		}
	}
	return packages, nil
}

func stringSlice(value any) ([]string, bool) {
	items, ok := value.([]any)
	if !ok {
		return nil, false
	}
	result := make([]string, len(items))
	for index, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, false
		}
		result[index] = text
	}
	return result, true
}

func MatchesAny(path string, patterns []string) bool {
	for _, pattern := range patterns {
		matched, err := doublestar.Match(pattern, path)
		if err == nil && matched {
			return true
		}
	}
	return false
}
