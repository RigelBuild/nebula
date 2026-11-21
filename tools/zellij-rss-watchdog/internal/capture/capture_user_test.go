package capture

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/zellij-rss-watchdog/internal/command"
	"github.com/RigelBuild/nebula/tools/zellij-rss-watchdog/internal/seam"
)

// runFake is the one seam method RunAsUser calls; every other seam method is unused here.
type runFake struct {
	seam.IO
	cfg   seam.Config
	runFn func(ctx context.Context, name string, args []string, timeout time.Duration) (command.Result, error)
}

func (f *runFake) Config() seam.Config { return f.cfg }

func (f *runFake) Run(ctx context.Context, name string, args []string, timeout time.Duration) (command.Result, error) {
	return f.runFn(ctx, name, args, timeout)
}

func TestUserTimeoutArgvBoundsAChildThatIgnoresSIGTERM(t *testing.T) {
	// runuser execs this argv as example-user, so the same binary and flags bound that uid's child.
	if len(userTimeoutArgv) == 0 {
		t.Fatal("userTimeoutArgv is empty")
	}
	last := userTimeoutArgv[len(userTimeoutArgv)-1]
	seconds, err := strconv.Atoi(last)
	if err != nil {
		t.Fatalf("last userTimeoutArgv value %q is not a number: %v", last, err)
	}
	if seconds >= int(command.CommandTimeout/time.Second) {
		t.Fatalf("user timeout %ds must be below the command timeout %s", seconds, command.CommandTimeout)
	}
	// Swap the bound for 1s so the test is fast; the flag shape is what is under test.
	args := append([]string(nil), userTimeoutArgv[1:len(userTimeoutArgv)-1]...)
	args = append(args, "1", "bash", "-c", "trap '' TERM; sleep 60")
	started := time.Now()
	_, err = command.Text(context.Background(), userTimeoutArgv[0], args, 10*time.Second)
	if err == nil {
		t.Fatal("expected the child to be killed")
	}
	if !strings.Contains(err.Error(), "command exited with status 137") && !strings.Contains(err.Error(), "command exited with status SIGKILL") {
		t.Fatalf("err = %v, want exit 137 or SIGKILL", err)
	}
	if elapsed := time.Since(started); elapsed >= 5*time.Second {
		t.Fatalf("elapsed = %s, want under 5s", elapsed)
	}
}

func TestUserCommandKilledByItsOwnTimeoutIsReportedAsTimeout(t *testing.T) {
	for _, status := range []string{"124", "137", "SIGKILL"} {
		io := &runFake{cfg: seam.Config{User: "example-user"}, runFn: func(context.Context, string, []string, time.Duration) (command.Result, error) {
			return command.Result{}, &command.Error{Status: status}
		}}
		_, err := RunAsUser(context.Background(), io, []string{"journalctl"})
		if err == nil || !strings.Contains(err.Error(), "command timed out after 25s as example-user") {
			t.Fatalf("status %s: err = %v, want user timeout", status, err)
		}
	}
	failing := &runFake{cfg: seam.Config{User: "example-user"}, runFn: func(context.Context, string, []string, time.Duration) (command.Result, error) {
		return command.Result{}, &command.Error{Status: "1", Stderr: "no journal"}
	}}
	_, err := RunAsUser(context.Background(), failing, []string{"journalctl"})
	if err == nil || !strings.Contains(err.Error(), "command exited with status 1; stderr=no journal") {
		t.Fatalf("err = %v, want the original status-1 error", err)
	}
}
