// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/RigelBuild/nebula/tools/go-lib/serviceurls"
	"github.com/RigelBuild/nebula/tools/service-urls/config"
	"go.yaml.in/yaml/v4"
)

type glanceConfig struct {
	Pages []glancePage `yaml:"pages"`
}

type glancePage struct {
	Name    string         `yaml:"name"`
	Columns []glanceColumn `yaml:"columns"`
}

type glanceColumn struct {
	Size    string         `yaml:"size"`
	Widgets []glanceWidget `yaml:"widgets"`
}

type glanceWidget struct {
	Type   string        `yaml:"type"`
	Groups []glanceGroup `yaml:"groups"`
}

type glanceGroup struct {
	Title string       `yaml:"title"`
	Links []glanceLink `yaml:"links"`
}

func glanceDescription(value string) *yaml.Node {
	node := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
	if strings.Contains(value, ",") {
		node.Style = yaml.DoubleQuotedStyle
	}
	return node
}

type glanceLink struct {
	Title       string     `yaml:"title"`
	URL         string     `yaml:"url"`
	Description *yaml.Node `yaml:"description"`
}

func renderGlance(inventory serviceurls.Inventory, cfg config.Config) (string, error) {
	groups := make([]glanceGroup, 0, len(inventory.Groups))
	for _, group := range inventory.Groups {
		links := make([]glanceLink, 0)
		for _, link := range inventory.Links {
			if link.Group == group.ID && link.Kind == serviceurls.KindWeb && link.Source != cfg.PortalSource {
				description := glanceDescription(link.What)
				links = append(links, glanceLink{Title: link.Name, URL: link.URL, Description: description})
			}
		}
		if len(links) > 0 {
			groups = append(groups, glanceGroup{Title: group.Title, Links: links})
		}
	}
	config := glanceConfig{Pages: []glancePage{{
		Name: "Internal links",
		Columns: []glanceColumn{{
			Size:    "full",
			Widgets: []glanceWidget{{Type: "bookmarks", Groups: groups}},
		}},
	}}}
	var output bytes.Buffer
	// Unlimited width keeps v3's unwrapped output; v4 encoders wrap at 80 columns.
	dumper, err := yaml.NewDumper(&output, yaml.WithV3Defaults(), yaml.WithIndent(2), yaml.WithLineWidth(-1))
	if err != nil {
		return "", fmt.Errorf("create Glance config dumper: %w", err)
	}
	if err := dumper.Dump(config); err != nil {
		return "", fmt.Errorf("encode Glance config: %w", err)
	}
	if err := dumper.Close(); err != nil {
		return "", fmt.Errorf("close Glance config encoder: %w", err)
	}
	return "# Generated from " + cfg.InventoryPath + "/. Do not edit by hand.\n" + output.String(), nil
}
