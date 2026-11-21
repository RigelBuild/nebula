// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"fmt"
	"strings"

	"github.com/RigelBuild/nebula/tools/go-lib/serviceurls"
	"github.com/RigelBuild/nebula/tools/service-urls/config"
)

func renderPage(inventory serviceurls.Inventory, cfg config.Config) (string, error) {
	var sections []string
	sections = append(sections,
		"# Internal service links",
		"",
		"Generated from [`"+cfg.InventoryPath+"/`](./"+cfg.InventoryPath+"/). Do not edit by hand.",
	)
	for _, group := range inventory.Groups {
		sections = append(sections,
			"",
			"## "+escapeCell(group.Title),
			"",
			"| Link | What | Gate | Port | Node | Via | Source |",
			"| --- | --- | --- | ---: | --- | --- | --- |",
		)
		for _, link := range inventory.Links {
			if link.Group != group.ID {
				continue
			}
			row, err := renderLink(link)
			if err != nil {
				return "", err
			}
			sections = append(sections, row)
		}
	}
	return strings.Join(sections, "\n") + "\n", nil
}

func renderLink(link serviceurls.Link) (string, error) {
	url := escapeCell(link.URL)
	if link.Kind != serviceurls.KindEndpoint {
		url = "[" + escapeCell(escapeLinkLabel(link.Name)) + "](<" + link.URL + ">)"
	}
	gate := string(link.Access)
	if link.Login != "" {
		gate += " + " + link.Login
	}
	_, port, err := serviceurls.LinkHost(link)
	if err != nil {
		return "", fmt.Errorf("get host and port for %q: %w", link.URL, err)
	}
	node := escapeCell(link.Node)
	source := ""
	if link.Source != "" {
		source = "`" + escapeCell(link.Source) + "`"
	}
	return fmt.Sprintf("| %s | %s | %s | %d | %s | %s | %s |", url, escapeCell(link.What), escapeCell(gate), port, node, escapeCell(link.Via), source), nil
}

func escapeCell(value string) string {
	value = strings.ReplaceAll(value, "|", "\\|")
	value = strings.ReplaceAll(value, "\r\n", " ")
	value = strings.ReplaceAll(value, "\r", " ")
	return strings.ReplaceAll(value, "\n", " ")
}

func escapeLinkLabel(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "[", "\\[")
	return strings.ReplaceAll(value, "]", "\\]")
}
