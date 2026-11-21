package watchdog

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/RigelBuild/nebula/tools/zellij-rss-watchdog/internal/capture"
	"github.com/RigelBuild/nebula/tools/zellij-rss-watchdog/internal/proc"
	"github.com/RigelBuild/nebula/tools/zellij-rss-watchdog/internal/seam"
)

// Run is the testable entry point: it samples until ctx is cancelled and returns an exit code.
func Run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	cfg, err := parseConfig(args, getenv)
	if err != nil {
		if _, werr := fmt.Fprintf(stderr, "zellij-rss-watchdog: %v\n", err); werr != nil {
			slog.Error("writing usage error", "error", werr)
		}
		return 2
	}
	return runWatchdog(ctx, newSystemIO(cfg, stdout, stderr))
}

// parseConfig reads the required --user, --session and --journal-unit flags. None has a
// default: the user, session name and journal unit belong to the operator's host. The log directory follows the
// uid, and the runtime dir follows XDG_RUNTIME_DIR, with the uid as the fallback.
func parseConfig(args []string, getenv func(string) string) (seam.Config, error) {
	flags := flag.NewFlagSet("zellij-rss-watchdog", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	user := flags.String("user", "", "account that owns the zellij server (required)")
	session := flags.String("session", "", "zellij session name whose server is sampled (required)")
	journal := flags.String("journal-unit", "", "user systemd unit whose journal is captured (required)")
	if err := flags.Parse(args); err != nil {
		return seam.Config{}, fmt.Errorf("parsing flags: %w", err)
	}
	if flags.NArg() != 0 {
		return seam.Config{}, fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	if *user == "" || *session == "" || *journal == "" {
		return seam.Config{}, errors.New("--user, --session and --journal-unit are required")
	}
	uid := os.Getuid()
	runtimeDir := getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		runtimeDir = fmt.Sprintf("/run/user/%d", uid)
	}
	return seam.Config{
		ProcRoot:       "/proc",
		RuntimeDir:     runtimeDir,
		CaptureRoot:    "/var/lib/zellij-rss-watchdog",
		RelaunchBinary: getenv("WAVE_SUPERVISOR_RELAUNCH"),
		User:           *user,
		Session:        *session,
		LogDirectory:   fmt.Sprintf("/tmp/zellij-%d/zellij-log", uid),
		JournalUnit:    *journal,
	}, nil
}

// runWatchdog samples the session server every sampleIntervalMS until ctx is cancelled.
func runWatchdog(ctx context.Context, io seam.IO) int {
	if err := io.Mkdir(io.Config().CaptureRoot); err != nil {
		io.Log("error", sampleFailed(err))
		return 1
	}
	for {
		if err := sampleOnce(ctx, io); err != nil {
			io.Log("error", sampleFailed(err))
		}
		if err := io.Sleep(ctx, time.Duration(sampleIntervalMS)*time.Millisecond); err != nil {
			return 0
		}
	}
}

// sampleFailed is the event=sample_failed log line for a sampling error.
func sampleFailed(err error) string {
	return "event=sample_failed message=" + capture.Logfmt(err.Error())
}

// sampleOnce takes one sample of the session server and acts on the decision.
// A sampling failure is logged and swallowed; only state I/O errors return.
func sampleOnce(ctx context.Context, io seam.IO) error {
	identity := io.Identity()
	switch identity {
	case "none":
		io.RecordUnavailableTransition(identity)
		return nil
	case proc.IdentityUnavailable:
		if io.RecordUnavailableTransition(identity) {
			io.Log("error", "event=identity_unavailable")
		}
		return nil
	}
	io.RecordUnavailableTransition(identity)
	statePath := filepath.Join(io.Config().CaptureRoot, stateFile)
	previous := loadState(io, statePath)
	if previous != nil && previous.Identity != identity {
		if err := saveState(io, statePath, watchState{Identity: identity, Armed: true, Observations: []observation{}}); err != nil {
			return err
		}
	}
	pid, err := proc.IdentityPid(identity)
	if err != nil {
		return err
	}
	if io.Identity() != identity {
		io.Log("error", "event=identity_changed message=server identity changed before status read")
		return nil
	}
	rssBytes, swapBytes, ok := readSample(io, pid)
	if !ok {
		return nil
	}
	now := io.Now()
	decided := decideSample(loadState(io, statePath), sample{
		time:      now,
		identity:  identity,
		rssBytes:  rssBytes,
		swapBytes: swapBytes,
	})
	if err := saveState(io, statePath, decided.state); err != nil {
		return err
	}
	if decided.capture {
		if err := capture.CaptureServer(ctx, io, identity, rssBytes); err != nil {
			return err
		}
	}
	if !decided.shouldRequest {
		return nil
	}
	return requestRestart(ctx, io, statePath, identity, now, decided.state)
}

// readSample reads the server's VmRSS and VmSwap, logging and reporting a failure as !ok.
func readSample(io seam.IO, pid string) (rssBytes, swapBytes int64, ok bool) {
	status, err := io.ReadText(io.Config().ProcRoot + "/" + pid + "/status")
	if err != nil {
		io.Log("error", sampleFailed(err))
		return 0, 0, false
	}
	rssBytes, err = parseVmRss(status)
	if err != nil {
		io.Log("error", sampleFailed(err))
		return 0, 0, false
	}
	swapBytes, err = parseVmSwap(status)
	if err != nil {
		io.Log("error", sampleFailed(err))
		return 0, 0, false
	}
	return rssBytes, swapBytes, true
}

// requestRestart asks the relaunch tool to restart the server as the configured user, then records the request time.
// Any failure, including the state write, is logged as restart_request_failed.
func requestRestart(ctx context.Context, io seam.IO, statePath, identity string, now int64, state watchState) error {
	relaunch := io.Config().RelaunchBinary
	if len(relaunch) == 0 || relaunch[0] != '/' {
		io.Log("error", "event=restart_request_failed message=absolute relaunch path unavailable")
		return nil
	}
	err := func() error {
		if _, err := capture.RunAsUser(ctx, io, []string{
			capture.EnvCommand,
			"XDG_RUNTIME_DIR=" + io.Config().RuntimeDir,
			"DBUS_SESSION_BUS_ADDRESS=unix:path=" + io.Config().RuntimeDir + "/bus",
			relaunch,
			"request-zellij-restart",
			"--reason",
			"rss",
			"--rss",
			identity,
		}); err != nil {
			return err
		}
		state.LastRequestAt = &now
		return saveState(io, statePath, state)
	}()
	if err != nil {
		io.Log("error", "event=restart_request_failed message="+capture.Logfmt(err.Error()))
		return nil
	}
	io.Log("info", "event=restart_requested identity="+identity)
	return nil
}
