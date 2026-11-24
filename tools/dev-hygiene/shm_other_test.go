//go:build !linux

package main

import (
	"bytes"
	"runtime"
	"strings"
	"testing"
)

func TestRunSkipsShmOffLinux(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runIsolated(t, []string{"-shm-only", "-to", testRecipient}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run returned %d: %s", code, stderr.String())
	}
	want := "shm: skipped (no /dev/shm on " + runtime.GOOS + ")"
	if !strings.Contains(stdout.String(), want) {
		t.Fatalf("output %q does not contain skip line %q", stdout.String(), want)
	}
}
