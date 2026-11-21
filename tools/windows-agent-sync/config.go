package main

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"
)

// schemaCUE holds the #Layout and #LinkTable definitions. The layout and the
// link table are caller input, checked against these schemas before use.
//
//go:embed schema.cue
var schemaCUE []byte

// layout is the decoded --layout file. Every path is relative to --repo-root.
type layout struct {
	LinkTable  string `json:"linkTable"`
	Pins       string `json:"pins"`
	NebulaLock string `json:"nebulaLock"`
	MCP        string `json:"mcp"`
	SecretMap  string `json:"secretMap"`
}

// checkout is the Windows repository named by --repo-root and the layout's
// paths inside it.
type checkout struct {
	root   string
	layout layout
}

// path returns the absolute path of a repo-relative, slash-separated path.
func (co checkout) path(rel string) string {
	return filepath.Join(co.root, filepath.FromSlash(rel))
}

// loadLayout reads and validates the --layout file.
func loadLayout(path string) (layout, error) {
	ctx := cuecontext.New()
	schema := ctx.CompileBytes(schemaCUE).LookupPath(cue.ParsePath("#Layout"))
	data, err := os.ReadFile(path) //nolint:gosec // The operator selects the layout file with --layout.
	if err != nil {
		return layout{}, fmt.Errorf("read layout: %w", err)
	}
	compiled := ctx.CompileBytes(data)
	if err := compiled.Err(); err != nil {
		return layout{}, fmt.Errorf("compile layout: %w", err)
	}
	value := compiled.Unify(schema)
	if err := value.Validate(cue.Concrete(true)); err != nil {
		return layout{}, fmt.Errorf("validate layout: %w", err)
	}
	var result layout
	if err := value.Decode(&result); err != nil {
		return layout{}, fmt.Errorf("decode layout: %w", err)
	}
	for _, field := range []struct{ name, rel string }{
		{"linkTable", result.LinkTable},
		{"pins", result.Pins},
		{"nebulaLock", result.NebulaLock},
		{"mcp", result.MCP},
		{"secretMap", result.SecretMap},
	} {
		if !filepath.IsLocal(filepath.FromSlash(field.rel)) {
			return layout{}, fmt.Errorf("layout %s must be a path inside --repo-root: %q", field.name, field.rel)
		}
	}
	return result, nil
}

// loadLinks reads and validates the link table at path.
func loadLinks(path string) ([]link, error) {
	ctx := cuecontext.New()
	schema := ctx.CompileBytes(schemaCUE).LookupPath(cue.ParsePath("#LinkTable"))
	data, err := os.ReadFile(path) //nolint:gosec // The layout names the link table inside --repo-root.
	if err != nil {
		return nil, fmt.Errorf("read link table: %w", err)
	}
	compiled := ctx.CompileBytes(data)
	if err := compiled.Err(); err != nil {
		return nil, fmt.Errorf("compile link table: %w", err)
	}
	table := compiled.Unify(schema)
	if err := table.Validate(cue.Concrete(true)); err != nil {
		return nil, fmt.Errorf("validate link table: %w", err)
	}
	iterator, err := table.LookupPath(cue.ParsePath("links")).List()
	if err != nil {
		return nil, fmt.Errorf("links: %w", err)
	}
	var links []link
	for iterator.Next() {
		var entry link
		if err := iterator.Value().Decode(&entry); err != nil {
			return nil, fmt.Errorf("decode link: %w", err)
		}
		links = append(links, entry)
	}
	return links, nil
}
