// SPDX-License-Identifier: MIT OR Apache-2.0

// Package main implements the dl-list executable.
package main

import (
	"context"
	"os"

	"github.com/RigelBuild/nebula/tools/dl-list/dllist"
)

func main() {
	os.Exit(dllist.Main(context.Background(), os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}
