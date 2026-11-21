package watchdog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/zellij-rss-watchdog/internal/seam"
)

// mkdirFailIO fails the capture-root mkdir, which is the only Mkdir runWatchdog makes.
type mkdirFailIO struct {
	*fakeIO
}

func (f *mkdirFailIO) Mkdir(string) error { return errors.New("read-only filesystem") }

// cancelledSleepIO reports the context as done on its first sleep, so the loop must exit.
type cancelledSleepIO struct {
	*fakeIO
}

func (f *cancelledSleepIO) Sleep(context.Context, time.Duration) error { return context.Canceled }

func TestRunRejectsMissingOrExtraFlagsWithExitTwo(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "no flags", args: nil, want: "--user, --session and --journal-unit are required"},
		{name: "missing user", args: []string{"--session", "example-session", "--journal-unit", "example-unit"}, want: "--user, --session and --journal-unit are required"},
		{name: "missing session", args: []string{"--user", "example-user", "--journal-unit", "example-unit"}, want: "--user, --session and --journal-unit are required"},
		{name: "missing journal unit", args: []string{"--user", "example-user", "--session", "example-session"}, want: "--user, --session and --journal-unit are required"},
		{name: "positional argument", args: []string{"--user", "example-user", "--session", "example-session", "extra"}, want: "unexpected arguments"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			code := Run(context.Background(), tc.args, func(string) string { return "" }, &bytes.Buffer{}, &stderr)
			if code != 2 {
				t.Fatalf("Run returned %d, want 2", code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), tc.want)
			}
		})
	}
}

func TestParseConfigBuildsTheSessionPaths(t *testing.T) {
	cfg, err := parseConfig([]string{"--user", testUser, "--session", testSession, "--journal-unit", testJournalUnit}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.User != testUser || cfg.Session != testSession || cfg.JournalUnit != testJournalUnit {
		t.Fatalf("user, session = %q, %q", cfg.User, cfg.Session)
	}
	want := fmt.Sprintf("/tmp/zellij-%d/zellij-log", os.Getuid())
	if cfg.LogDirectory != want {
		t.Fatalf("LogDirectory = %q, want %q", cfg.LogDirectory, want)
	}
}

func TestRunWatchdogExitsOneWhenTheCaptureRootCannotBeCreated(t *testing.T) {
	io := &mkdirFailIO{fakeIO: newFakeIO()}
	if code := runWatchdog(context.Background(), io); code != 1 {
		t.Fatalf("runWatchdog returned %d, want 1", code)
	}
	if !io.logged("error", "event=sample_failed") {
		t.Fatal("a failed capture-root mkdir was not logged as sample_failed")
	}
}

func TestRunWatchdogExitsZeroWhenTheContextIsCancelled(t *testing.T) {
	io := &cancelledSleepIO{fakeIO: newFakeIO()}
	if code := runWatchdog(context.Background(), io); code != 0 {
		t.Fatalf("runWatchdog returned %d after cancellation, want 0", code)
	}
}

func TestRequestRestartRefusesARelativeRelaunchPath(t *testing.T) {
	io := newFakeIO()
	io.cfg.RelaunchBinary = "wave-supervisor-relaunch"
	err := requestRestart(context.Background(), io, "/state.json", "123:456", fakeNowMS, watchState{})
	if err != nil {
		t.Fatalf("requestRestart returned %v, want nil for a refused relative path", err)
	}
	if !io.logged("error", "absolute relaunch path unavailable") {
		t.Fatal("a relative relaunch path was not refused with the absolute-path message")
	}
}

// Compile-time check that the test wrappers still satisfy seam.IO.
var (
	_ seam.IO = (*mkdirFailIO)(nil)
	_ seam.IO = (*cancelledSleepIO)(nil)
)
