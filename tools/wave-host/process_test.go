package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestParseLinuxStat(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		bad               bool
	}{
		{name: "name containing parentheses", input: "42 (agent ) worker) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 987 20", want: "987"},
		{name: "short record", input: "42 (name) S 1", bad: true},
		{name: "invalid token", input: "42 (name) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 no 20", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseLinuxStat(tc.input)
			if (err != nil) != tc.bad || got != tc.want {
				t.Fatalf("got %q, %v", got, err)
			}
		})
	}
}

func TestIdentifyCurrentProcess(t *testing.T) {
	id, err := Identify(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if id.PID != os.Getpid() || id.StartToken == "" {
		t.Fatalf("invalid identity: %+v", id)
	}
}

func TestIdentityReadError(t *testing.T) {
	readErr := errors.New("read failed")
	if err := identityReadError(os.Getpid(), readErr); !errors.Is(err, readErr) {
		t.Fatalf("live process error: %v", err)
	}
	child := startProcessHelper(t)
	pid := child.cmd.Process.Pid
	if err := child.stop(); err != nil {
		t.Fatal(err)
	}
	if err := identityReadError(pid, readErr); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reaped process error: %v", err)
	}
}

func TestProcessIdentityHelper(t *testing.T) {
	if os.Getenv("WAVE_HOST_PROCESS_HELPER") != "1" {
		return
	}
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatal(err)
	}
}

type processTestChild struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
}

func startProcessHelper(t *testing.T) *processTestChild {
	t.Helper()
	child, err := startProcessHelperCommand()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := child.stop(); err != nil {
			t.Error(err)
		}
	})
	return child
}

func startProcessHelperCommand() (*processTestChild, error) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessIdentityHelper$")
	cmd.Env = append(os.Environ(), "WAVE_HOST_PROCESS_HELPER=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("create helper stdin: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(fmt.Errorf("start process helper: %w", err), stdin.Close())
	}
	return &processTestChild{cmd: cmd, stdin: stdin}, nil
}

func (child *processTestChild) stop() error {
	var closeErr error
	if child.stdin != nil {
		closeErr = child.stdin.Close()
		child.stdin = nil
	}
	var waitErr error
	if child.cmd.ProcessState == nil {
		waitErr = child.cmd.Wait()
	}
	return errors.Join(closeErr, waitErr)
}

func TestParseDarwinIdentity(t *testing.T) {
	id, err := darwinIdentity(42, 1700000000, 123456)
	if err != nil || id.StartToken != "1700000000123456" {
		t.Fatalf("got %+v, %v", id, err)
	}
	if _, err := darwinIdentity(42, 0, 1000000); err == nil {
		t.Fatal("accepted invalid start time")
	}
}

func TestIdentifyDarwinReader(t *testing.T) {
	for _, tc := range []struct {
		name   string
		actual int
		err    error
		bad    bool
	}{
		{name: "valid", actual: 42},
		{name: "wrong pid", actual: 43, bad: true},
		{name: "unreadable", err: errors.New("denied"), bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, err := identifyDarwin(42, func(pid int) (int, int64, int64, error) {
				return tc.actual, 1700000000, 123456, tc.err
			})
			if (err != nil) != tc.bad || (!tc.bad && id.StartToken != "1700000000123456") {
				t.Fatalf("got %+v, %v", id, err)
			}
		})
	}
}

func TestSocketPath(t *testing.T) {
	for _, tc := range []struct {
		name, platform, dir string
		bad                 bool
	}{
		{name: "darwin exact", platform: "darwin", dir: "/" + strings.Repeat("a", 95)},
		{name: "darwin long", platform: "darwin", dir: "/" + strings.Repeat("a", 96), bad: true},
		{name: "linux exact", platform: "linux", dir: "/" + strings.Repeat("a", 99)},
		{name: "linux long", platform: "linux", dir: "/" + strings.Repeat("a", 100), bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := socketPath("x", tc.dir, tc.platform)
			if (err != nil) != tc.bad {
				t.Fatalf("got %v", err)
			}
		})
	}
}
