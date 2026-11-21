package command

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// oversizedBytes is 65 MiB, past the 64 MiB output cap.
const oversizedBytes = 65 * 1024 * 1024

func TestHungChildCommandIsKilledAndReportedAsTimeout(t *testing.T) {
	_, err := Text(context.Background(), "sleep", []string{"5"}, 100*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "command timed out after 0.1s") {
		t.Fatalf("err = %v, want timeout after 0.1s", err)
	}
}

func TestTextCommandDistinguishesOutputLimitFromTimeout(t *testing.T) {
	// head -c emits more than the output cap without a shell pipeline.
	// 60s covers a race-mode 64 MiB transfer on a loaded host and still bounds a hang.
	_, err := Text(context.Background(), "head", []string{"-c", strconv.Itoa(oversizedBytes), "/dev/zero"}, 60*time.Second)
	if err == nil || !strings.Contains(err.Error(), "command output exceeded maxBuffer") {
		t.Fatalf("err = %v, want output-limit error", err)
	}
	if timeout, ok := errors.AsType[*TimeoutError](err); ok {
		t.Fatalf("output-limit overflow reported as a timeout: %v (%s)", err, timeout.Error())
	}
}
