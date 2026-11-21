// Package scenario runs the pod, host, and AKS log-cap checks against real Alloy.
package scenario

import (
	"context"
	"fmt"
	"io"
	"log/slog"
)

const (
	mb             = 1_000_000
	sustainSeconds = 30
	labelService   = "service_name"
	streamFlood    = "flood"
)

// Harness holds the argv paths, the output sink, and the shared check ledger.
type Harness struct {
	Out        io.Writer
	PodAlloy   string
	HostAlloy  string
	PodYAML    string
	HostConfig string
	AKSYAML    string
	failures   []string
}

// WriteLine writes one output line. A failed write is logged, not returned.
func WriteLine(w io.Writer, s string) {
	if _, err := fmt.Fprintln(w, s); err != nil {
		slog.Warn("write output", "err", err)
	}
}

// Failures returns how many checks failed.
func (h *Harness) Failures() int {
	return len(h.failures)
}

// Run runs the pod, host, and AKS scenarios in order. A scenario that cannot
// run stops the sequence; a failed check only records a failure.
func (h *Harness) Run(ctx context.Context) error {
	if err := h.podScenario(ctx); err != nil {
		return err
	}
	if err := h.hostScenario(ctx); err != nil {
		return err
	}
	return h.aksScenario(ctx)
}

// check records one assertion and prints it in the harness's line format.
func (h *Harness) check(ok bool, what string) {
	if ok {
		WriteLine(h.Out, "ok   "+what)
		return
	}
	WriteLine(h.Out, "FAIL "+what)
	h.failures = append(h.failures, what)
}
