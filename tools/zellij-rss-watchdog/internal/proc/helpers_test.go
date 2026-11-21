//go:build linux

package proc

import (
	"testing"
	"time"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// requireUntil fails the test unless condition holds within limit.
func requireUntil(t *testing.T, limit time.Duration, what string, condition func() bool) {
	t.Helper()
	if !waitUntil(limit, condition) {
		t.Fatalf("timed out after %s waiting for %s", limit, what)
	}
}

func waitUntil(limit time.Duration, condition func() bool) bool {
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return true
		}
		select {
		case <-deadline.C:
			return condition()
		case <-ticker.C:
		}
	}
}
