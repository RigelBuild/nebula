// SPDX-License-Identifier: MIT OR Apache-2.0

// Package main implements the dl-reconcile executable.
package main

import (
	"context"
	"os"

	"github.com/RigelBuild/nebula/tools/dl-reconcile/dlreconcile"
)

func main() {
	os.Exit(dlreconcile.Main(context.Background(), os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}
