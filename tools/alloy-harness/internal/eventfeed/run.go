package eventfeed

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/RigelBuild/nebula/tools/alloy-harness/internal/alloyproc"
	"github.com/RigelBuild/nebula/tools/alloy-harness/internal/check"
)

const (
	pollEvery = 500 * time.Millisecond
	// The positive row normally lands well inside this; it bounds a stuck Alloy.
	positiveDeadline = 120 * time.Second
	// A leaked row ships after the positive row, so absence is checked after this settle.
	settle = 5 * time.Second
)

// Run starts Alloy on the rendered feed pipeline, feeds it feedDir, and returns the
// assertions. It errors only when the harness itself fails, not when a check does.
func Run(ctx context.Context, alloy, configPath, feedDir string) ([]check.Check, error) {
	raw, err := alloyproc.ReadConfig(configPath)
	if err != nil {
		return nil, err
	}
	pipeline, err := Pipeline(raw, feedDir)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "alloy-event-feed-")
	if err != nil {
		return nil, fmt.Errorf("make temp dir: %w", err)
	}
	defer alloyproc.RemoveDir(dir)
	config := filepath.Join(dir, "config.alloy")
	if err := alloyproc.WriteConfig(config, ConfigFile(pipeline)); err != nil {
		return nil, err
	}
	port, err := alloyproc.FreePort()
	if err != nil {
		return nil, err
	}
	proc, err := alloyproc.Start(alloy, dir, config, port)
	if err != nil {
		return nil, err
	}
	exited, echoed, runErr := observe(ctx, proc)
	if err := errors.Join(runErr, proc.Stop()); err != nil {
		return nil, err
	}
	return Assert(exited, echoed), nil
}

// observe polls until the positive row is echoed, Alloy exits, or the deadline passes.
// It then settles so a leaked row would also have time to appear, and returns the log.
func observe(ctx context.Context, proc *alloyproc.Proc) (exited bool, echoed string, err error) {
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	deadline := time.NewTimer(positiveDeadline)
	defer deadline.Stop()
wait:
	for {
		select {
		case <-ctx.Done():
			return false, "", ctx.Err()
		case <-proc.Done():
			exited = true
			break wait
		case <-deadline.C:
			break wait
		case <-tick.C:
			echoed, err = alloyproc.ReadLog(proc.LogPath)
			if err != nil {
				return false, "", err
			}
			if Done(echoed) {
				break wait
			}
		}
	}
	settled := time.NewTimer(settle)
	defer settled.Stop()
	select {
	case <-ctx.Done():
		return exited, "", ctx.Err()
	case <-settled.C:
	}
	echoed, err = alloyproc.ReadLog(proc.LogPath)
	return exited, echoed, err
}
