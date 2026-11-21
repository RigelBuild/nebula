// wave-seat-holder runs each agent in its own tmux server under a transient systemd user unit.
package main

import (
	"context"
	"os"

	"github.com/RigelBuild/nebula/tools/wave-seat-holder/internal/seat"
)

func main() {
	os.Exit(seat.Run(context.Background(), os.Args[1:], seat.ProductionDeps(os.LookupEnv, os.Stdout, os.Stderr)))
}
