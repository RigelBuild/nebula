//go:build !linux

// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "fleet-gc-roots: Linux only")
	os.Exit(2)
}
