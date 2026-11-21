// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	_ "embed" // schema.cue is compiled into the binary; the config file is checked against it.
	"errors"
	"fmt"
	"os"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/ast"
	"cuelang.org/go/cue/cuecontext"
	"cuelang.org/go/cue/parser"
)

//go:embed schema.cue
var schemaSource []byte

var errImports = errors.New("imports are not permitted")

// config holds the deployment values. The binary has no default for any of them.
type config struct {
	Repo  string   `json:"repo"`
	Flake string   `json:"flake"`
	Hosts []string `json:"hosts"`
}

func loadConfig(path string) (config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // The config path is an operator-supplied flag.
	if err != nil {
		return config{}, fmt.Errorf("read config: %w", err)
	}
	return decodeConfig(data)
}

func decodeConfig(data []byte) (config, error) {
	ctx := cuecontext.New()
	schema := ctx.CompileBytes(schemaSource)
	if err := schema.Err(); err != nil {
		return config{}, fmt.Errorf("compile embedded schema: %w", err)
	}
	file, err := parser.ParseFile("config.cue", data)
	if err != nil {
		return config{}, fmt.Errorf("parse config: %w", err)
	}
	for _, declaration := range file.Decls {
		if _, ok := declaration.(*ast.ImportDecl); ok {
			return config{}, errImports
		}
	}
	value := ctx.BuildFile(file).Unify(schema.LookupPath(cue.MakePath(cue.Def("Config"))))
	if err := value.Validate(cue.Concrete(true)); err != nil {
		return config{}, fmt.Errorf("validate config: %w", err)
	}
	var cfg config
	if err := value.Decode(&cfg); err != nil {
		return config{}, fmt.Errorf("decode config: %w", err)
	}
	return cfg, nil
}
