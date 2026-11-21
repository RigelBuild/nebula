// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/RigelBuild/nebula/tools/go-lib/serviceurls"
	"github.com/RigelBuild/nebula/tools/service-urls/config"
)

func readInventory(root string, cfg config.Config) (serviceurls.Inventory, error) {
	inventory, err := serviceurls.ReadInventory(filepath.Join(root, filepath.FromSlash(cfg.InventoryPath)), serviceurls.Options{
		InternalHostSuffixes: cfg.InternalHostSuffixes,
	})
	if err != nil {
		return serviceurls.Inventory{}, fmt.Errorf("read %s: %w", cfg.InventoryPath, err)
	}
	return inventory, nil
}

func generate(root string, cfg config.Config) error {
	inventory, err := readInventory(root, cfg)
	if err != nil {
		return err
	}
	page, err := renderPage(inventory, cfg)
	if err != nil {
		return err
	}
	if err := writeGenerated(root, cfg.LinksPagePath, page); err != nil {
		return err
	}
	glance, err := renderGlance(inventory, cfg)
	if err != nil {
		return err
	}
	return writeGenerated(root, cfg.GlancePath, glance)
}

func writeGenerated(root, relative, contents string) error {
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil { //nolint:gosec // Generated docs use repository-relative paths and mode 0644.
		return fmt.Errorf("write %s: %w", relative, err)
	}
	return nil
}
