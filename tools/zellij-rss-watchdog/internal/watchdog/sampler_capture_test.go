package watchdog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/zellij-rss-watchdog/internal/capture"
	"github.com/RigelBuild/nebula/tools/zellij-rss-watchdog/internal/command"
)

func TestRecordsCommandTimeoutsInErrorsTxt(t *testing.T) {
	io := newFakeIO()
	errorsText := ""
	io.writeTextFn = func(path, text string) error {
		if strings.HasSuffix(path, "errors.txt") {
			errorsText = text
		}
		io.files[path] = text
		return nil
	}
	io.runFn = func(_ context.Context, name string, _ []string, timeout time.Duration) (command.Result, error) {
		switch name {
		case "ss":
			if timeout != command.CommandTimeout {
				t.Errorf("ss timeout = %v, want %v", timeout, command.CommandTimeout)
			}
			return command.Result{}, errors.New("command timed out after 30s")
		case "runuser", "bash":
			return command.Result{}, nil
		}
		return command.Result{}, fmt.Errorf("unexpected command: %s", name)
	}
	must(t, capture.CaptureServer(context.Background(), io, "123:456", 2*gib))
	if !strings.Contains(errorsText, "socket-peers: command timed out after 30s") {
		t.Fatalf("errors.txt = %q", errorsText)
	}
}

func TestRecordsMissingServerSocketRow(t *testing.T) {
	io := newFakeIO()
	errorsText := ""
	io.writeTextFn = func(path, text string) error {
		if strings.HasSuffix(path, "errors.txt") {
			errorsText = text
		}
		io.files[path] = text
		return nil
	}
	io.runFn = func(_ context.Context, name string, _ []string, _ time.Duration) (command.Result, error) {
		switch name {
		case "ss", "runuser", "bash":
			return command.Result{}, nil
		}
		return command.Result{}, fmt.Errorf("unexpected command: %s", name)
	}
	must(t, capture.CaptureServer(context.Background(), io, "123:456", 2*gib))
	if !strings.Contains(errorsText, "socket-peers: ss output has no server row") {
		t.Fatalf("errors.txt = %q", errorsText)
	}
}

func TestChangedServerStarttimeSkipsEveryProcStep(t *testing.T) {
	io := newFakeIO()
	var reads []string
	errorsText := ""
	identityReads := 0
	io.identityFn = func() string {
		identityReads++
		return "123:789"
	}
	io.readTextFn = func(path string) (string, error) {
		reads = append(reads, path)
		return "", nil
	}
	io.writeTextFn = func(path, text string) error {
		if strings.HasSuffix(path, "errors.txt") {
			errorsText = text
		}
		io.files[path] = text
		return nil
	}
	io.runFn = func(_ context.Context, name string, _ []string, _ time.Duration) (command.Result, error) {
		switch name {
		case "ss", "runuser", "bash":
			return command.Result{}, nil
		}
		return command.Result{}, fmt.Errorf("unexpected command: %s", name)
	}
	must(t, capture.CaptureServer(context.Background(), io, "123:456", 2*gib))
	for _, path := range reads {
		if strings.HasPrefix(path, "/proc/123/") {
			t.Fatalf("read %s after identity changed", path)
		}
	}
	if identityReads == 0 {
		t.Fatal("identity never re-checked")
	}
	if !strings.Contains(errorsText, "server identity changed") {
		t.Fatalf("errors.txt = %q", errorsText)
	}
}

func TestUnavailableIdentityLogsOncePerTransitionWithoutChangingState(t *testing.T) {
	io := newFakeIO()
	identity := "unavailable"
	writes := 0
	io.identityFn = func() string { return identity }
	io.readTextFn = func(string) (string, error) {
		return "", errors.New("unexpected proc read")
	}
	io.writeTextFn = func(string, string) error {
		writes++
		return nil
	}
	ctx := context.Background()
	must(t, sampleOnce(ctx, io))
	must(t, sampleOnce(ctx, io))
	identity = "none"
	must(t, sampleOnce(ctx, io))
	identity = "unavailable"
	must(t, sampleOnce(ctx, io))
	errorLogs := 0
	for _, entry := range io.logs {
		if entry.level == "error" {
			errorLogs++
		}
	}
	if errorLogs != 2 {
		t.Fatalf("error logs = %d, want 2", errorLogs)
	}
	if writes != 0 {
		t.Fatalf("writes = %d, want 0", writes)
	}
}
