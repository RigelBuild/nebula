package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type lifecycleCase struct {
	name        string
	initial     string
	present     bool
	unknown     bool
	failSession bool
	failCleanup bool
	stop        bool
	long        bool
	wantErr     bool
	wantState   string
}

func TestSeatLifecycle(t *testing.T) {
	cases := []lifecycleCase{
		{name: "live duplicate", initial: "running", present: true, wantErr: true, wantState: "running"},
		{name: "pending orphan", initial: "pending", present: true, wantState: "running"},
		{name: "unknown start", initial: "pending", unknown: true, wantErr: true, wantState: "pending"},
		{name: "unknown stop", initial: "pending", unknown: true, stop: true, wantErr: true, wantState: "pending"},
		{name: "failed wait", failSession: true, wantErr: true},
		{name: "failed cleanup", failSession: true, failCleanup: true, wantErr: true, wantState: "pending"},
		{name: "absent stop", initial: "pending", stop: true},
		{name: "long socket", long: true, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { runLifecycleCase(t, tc) })
	}
}

func runLifecycleCase(t *testing.T, tc lifecycleCase) {
	t.Helper()
	dir := shortTempDir(t)
	t.Setenv("HOME", dir)
	path := filepath.Join(dir, "seat.json")
	present := tc.present
	started := tc.present
	waiting := false
	boots := 0
	if tc.initial != "" {
		if err := saveSeat(path, seatRecord{State: tc.initial, Label: "build.rigel.wave-seat.seat", Socket: "/tmp/seat.sock", HolderPID: 123, HolderToken: "token"}); err != nil {
			t.Fatal(err)
		}
	}
	h := seatHost{dir: dir, domain: "gui/1", timeout: time.Second, connect: func(context.Context, string) error { return nil }, identify: func(pid int) (Identity, error) { return Identity{PID: pid, StartToken: "token"}, nil }, children: func(int) ([]int, error) { return []int{789}, nil }}
	launch := func(ctx context.Context, args ...string) ([]byte, error) {
		switch args[0] {
		case "print":
			if tc.unknown {
				return nil, errors.New("unknown")
			}
			if present {
				if waiting {
					waiting = false
					return []byte("state = waiting\n"), nil
				}
				if started {
					return []byte("pid = 123\n"), nil
				}
				return []byte("state = waiting\n"), nil
			}
			cmd := exec.CommandContext(ctx, "sh", "-c", "exit 113")
			err := cmd.Run()
			return []byte("Could not find service"), err
		case "bootout":
			if tc.failCleanup && boots > 0 {
				return nil, errors.New("cleanup failed")
			}
			present = false
			return nil, nil
		case "bootstrap":
			data, readErr := os.ReadFile(args[2])
			if readErr != nil {
				return nil, readErr
			}
			started = strings.Contains(string(data), "<key>RunAtLoad</key><true/>")
			waiting = true
			boots++
			present = true
			return nil, nil
		}
		return nil, errors.New("unexpected launchctl command")
	}
	h.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "launchctl" {
			return launch(ctx, args...)
		}
		if len(args) > 0 && args[0] == "-N" && strings.Contains(strings.Join(args, " "), "new-session") {
			// A regular file stands in for the socket node.
			if err := os.WriteFile(filepath.Join(dir, "agents/run/seats/seat.sock"), nil, 0600); err != nil {
				return nil, err
			}
			if tc.failSession {
				return nil, errors.New("session failed")
			}
			return nil, nil
		}
		return []byte("456\n"), nil
	}
	socket := filepath.Join(dir, "agents/run/seats/seat.sock")
	if tc.long {
		socket = "/" + strings.Repeat("x", 104)
	}
	var err error
	if tc.stop {
		err = h.stop(context.Background(), "seat")
	} else {
		_, _, err = h.start(context.Background(), seatStartArgs{name: "seat", socket: socket, conf: "conf", cwd: "/tmp", tmux: "tmux", command: []string{"agent"}})
	}
	checkLifecycleResult(t, tc, path, boots, err)
}

func checkLifecycleResult(t *testing.T, tc lifecycleCase, path string, boots int, err error) {
	t.Helper()
	if (err != nil) != tc.wantErr {
		t.Fatalf("error = %v", err)
	}
	record, readErr := loadSeat(path)
	if tc.wantState == "" {
		if !errors.Is(readErr, os.ErrNotExist) {
			t.Fatalf("record = %+v, %v", record, readErr)
		}
	} else if readErr != nil || record.State != tc.wantState {
		t.Fatalf("record = %+v, %v", record, readErr)
	}
	if tc.name == "pending orphan" && boots != 1 {
		t.Fatalf("bootstraps = %d", boots)
	}
}
