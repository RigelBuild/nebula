package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/RigelBuild/nebula/tools/zellij-rss-watchdog/internal/watchdog"
)

// main owns the process root context and hands it to watchdog.Run.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := watchdog.Run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
