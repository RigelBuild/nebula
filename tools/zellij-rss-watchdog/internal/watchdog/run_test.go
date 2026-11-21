package watchdog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
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

func TestRunRejectsBadFlagsWithExitTwo(t *testing.T) {
	tmp := t.TempDir()
	missing := filepath.Join(tmp, "absent")
	required := "zellij-rss-watchdog: " + requiredFlagsMessage + "\n"
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "no flags", args: nil, want: required},
		{name: "missing user", args: []string{"--session", "s", "--journal-unit", "u", "--client-tmpdir", tmp}, want: required},
		{name: "missing session", args: []string{"--user", "a", "--journal-unit", "u", "--client-tmpdir", tmp}, want: required},
		{name: "missing journal unit", args: []string{"--user", "a", "--session", "s", "--client-tmpdir", tmp}, want: required},
		{name: "missing client tmpdir", args: []string{"--user", "a", "--session", "s", "--journal-unit", "u"}, want: required},
		{name: "client tmpdir absent", args: []string{"--user", "a", "--session", "s", "--journal-unit", "u", "--client-tmpdir", missing}, want: "zellij-rss-watchdog: --client-tmpdir: stat " + missing + ": no such file or directory\n"},
		{name: "unknown flag", args: []string{"--once"}, want: "zellij-rss-watchdog: parsing flags: flag provided but not defined: -once\n"},
		{name: "positional argument", args: []string{"--user", "a", "extra"}, want: "zellij-rss-watchdog: unexpected arguments: [extra]\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			code := Run(t.Context(), tc.args, func(string) string { return "" }, &bytes.Buffer{}, &stderr)
			if code != 2 {
				t.Fatalf("Run returned %d, want 2", code)
			}
			if stderr.String() != tc.want {
				t.Fatalf("stderr = %q, want %q", stderr.String(), tc.want)
			}
		})
	}
}

// TestParseConfigBuildsPathsFromTheUserUID checks the paths follow the uid of --user,
// not the process uid: the tool runs as root.
func TestParseConfigBuildsPathsFromTheUserUID(t *testing.T) {
	tmp := t.TempDir()
	uidOf := func(name string) (string, error) {
		if name != testUser {
			return "", fmt.Errorf("unexpected user %q", name)
		}
		return "4242", nil
	}
	args := []string{"--user", testUser, "--session", testSession, "--journal-unit", testJournalUnit, "--client-tmpdir", tmp}
	cases := []struct {
		name        string
		xdg         string
		wantRuntime string
	}{
		{name: "XDG_RUNTIME_DIR unset", xdg: "", wantRuntime: "/run/user/4242"},
		{name: "XDG_RUNTIME_DIR set", xdg: "/example/runtime", wantRuntime: "/example/runtime"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(key string) string {
				if key == "XDG_RUNTIME_DIR" {
					return tc.xdg
				}
				return ""
			}
			cfg, err := parseConfig(args, getenv, uidOf)
			if err != nil {
				t.Fatalf("parseConfig: %v", err)
			}
			if cfg.User != testUser || cfg.Session != testSession || cfg.JournalUnit != testJournalUnit || cfg.ClientTmpDir != tmp {
				t.Fatalf("cfg = %+v", cfg)
			}
			if cfg.LogDirectory != "/tmp/zellij-4242/zellij-log" {
				t.Fatalf("LogDirectory = %q, want /tmp/zellij-4242/zellij-log", cfg.LogDirectory)
			}
			if cfg.RuntimeDir != tc.wantRuntime {
				t.Fatalf("RuntimeDir = %q, want %q", cfg.RuntimeDir, tc.wantRuntime)
			}
		})
	}
}

func TestParseConfigFailsForAnUnknownUser(t *testing.T) {
	args := []string{"--user", "example-absent-user", "--session", testSession, "--journal-unit", testJournalUnit, "--client-tmpdir", t.TempDir()}
	_, err := parseConfig(args, func(string) string { return "" }, lookupUID)
	if err == nil || !strings.Contains(err.Error(), `looking up --user "example-absent-user"`) {
		t.Fatalf("err = %v, want a --user lookup failure", err)
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
