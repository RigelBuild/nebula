// Command alloy-scrape-limits runs the Alloy binary on argv against a /metrics fixture: 65
// samples with sample_limit 64 are refused, and 64 scrape cleanly.
// Usage: alloy-scrape-limits <alloy-binary>
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/RigelBuild/nebula/tools/alloy-harness/internal/check"
	"github.com/RigelBuild/nebula/tools/alloy-harness/internal/scrapelimit"
)

const usage = "usage: alloy-scrape-limits <alloy-binary>"

func main() {
	// Process root: the one place a context is created, so signals reach the run.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := mainWith(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// mainWith checks argv, runs both scrape runs, and returns the process exit code.
func mainWith(ctx context.Context, args []string, stdout, stderr *os.File) int {
	if len(args) != 1 || args[0] == "" {
		return check.Usage(stderr, usage)
	}
	alloy := args[0]
	over, err := scrapelimit.Scrape(ctx, "over", alloy, 65, 64)
	if err != nil {
		slog.Error("alloy-scrape-limits failed", "err", err)
		return 1
	}
	at, err := scrapelimit.Scrape(ctx, "at", alloy, 64, 64)
	if err != nil {
		slog.Error("alloy-scrape-limits failed", "err", err)
		return 1
	}
	return check.Report(stdout, stderr, scrapelimit.Checks(over, at))
}
