// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"io"
	"os"

	"github.com/RigelBuild/nebula/tools/docs-bundle/bundle"
)

func run(args []string, stdout, stderr io.Writer) int {
	return bundle.Main(args, stdout, stderr)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// Tests exercise the same entrypoint used by main with captured output streams.
