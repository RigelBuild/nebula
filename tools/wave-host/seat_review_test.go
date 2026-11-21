package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func shortTempDir(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("/tmp", "wh") //nolint:usetesting // t.TempDir follows TMPDIR, too long for a 103-byte socket path.
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove temporary directory: %v", err)
		}
	})
	return dir
}

func TestSeatPlist(t *testing.T) {
	data, err := seatPlist("seat&x", "/tmux<&", "/sock", "/conf\"", "/logs", "/bin:/tools&more")
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"seat&amp;x", "/tmux&lt;&amp;", "/conf&#34;", "<key>RunAtLoad</key><true/>", "<key>TMPDIR</key><string>/tmp/</string>", "<key>PATH</key><string>/bin:/tools&amp;more</string>", "<key>StandardOutPath</key>", "<key>StandardErrorPath</key>", "<string>-D</string>"} {
		if !strings.Contains(data, part) {
			t.Errorf("plist missing %q: %s", part, data)
		}
	}
	if strings.Contains(data, "KeepAlive") {
		t.Fatal("plist contains KeepAlive")
	}
}

type reviewCase struct {
	name      string
	initial   string
	mode      string
	wantErr   bool
	wantState string
}

func TestSeatReviewCases(t *testing.T) {
	cases := []reviewCase{
		{name: "timeout cleans pending", mode: "timeout", wantErr: true},
		{name: "identity mismatch starts", initial: "running", mode: "mismatch", wantState: "running"},
		{name: "dead identity starts", initial: "running", mode: "dead", wantState: "running"},
		{name: "live agent refuses start", initial: "running", mode: "agent-alive", wantErr: true, wantState: "running"},
		{name: "unknown agent refuses start", initial: "running", mode: "agent-unknown", wantErr: true, wantState: "running"},
		{name: "unknown identity refuses", initial: "running", mode: "unknown", wantErr: true, wantState: "running"},
		{name: "invalid holder cleans", mode: "invalid", wantErr: true},
		{name: "stop still alive retains", initial: "running", mode: "alive-stop", wantErr: true, wantState: "running"},
		{name: "stop mismatch clears", initial: "running", mode: "mismatch-stop"},
		{name: "stop kill fails but processes gone", initial: "running", mode: "kill-fails-stop"},
		{name: "stop kill fails and identity unknown", initial: "running", mode: "kill-fails-unknown-stop", wantErr: true, wantState: "running"},
		{name: "stop unknown retains", initial: "running", mode: "unknown-stop", wantErr: true, wantState: "running"},
		{name: "orphan stop refuses", mode: "orphan-stop", wantErr: true},
		{name: "orphan start refuses", mode: "orphan-start", wantErr: true},
		{name: "bootstrap failure skips kill", mode: "bootstrap-failure", wantErr: true},
		{name: "outside home refuses", mode: "outside", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { runReviewCase(t, tc) })
	}
}

func runReviewCase(t *testing.T, tc reviewCase) {
	t.Helper()
	dir := shortTempDir(t)
	t.Setenv("HOME", dir)
	path := filepath.Join(dir, "seat.json")
	label := "build.rigel.wave-seat.seat"
	present := tc.initial != "" || strings.HasPrefix(tc.mode, "orphan-")
	booted := false
	bootouts := 0
	kills := 0
	if tc.initial != "" {
		err := saveSeat(path, seatRecord{State: tc.initial, Label: label, Socket: filepath.Join(dir, "agents/run/seats/seat.sock"), HolderPID: 123, HolderToken: "old", AgentPID: 789, AgentToken: "old"})
		if err != nil {
			t.Fatal(err)
		}
	}
	h := seatHost{dir: dir, domain: "gui/1", timeout: 20 * time.Millisecond, connect: func(context.Context, string) error { return nil }, children: func(int) ([]int, error) { return []int{789}, nil }}
	h.identify = reviewIdentify(tc.mode, &booted)
	h.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return reviewRun(ctx, name, args, tc.mode, &present, &booted, &bootouts, &kills)
	}
	socket := filepath.Join(dir, "agents/run/seats/seat.sock")
	if tc.mode == "outside" {
		socket = "/tmp/seat.sock"
	}
	var err error
	if strings.HasSuffix(tc.mode, "-stop") {
		err = h.stop(context.Background(), "seat")
	} else {
		_, _, err = h.start(context.Background(), seatStartArgs{name: "seat", socket: socket, conf: "conf", cwd: "/tmp", tmux: "tmux", command: []string{"agent"}})
	}
	if (err != nil) != tc.wantErr {
		t.Fatalf("error = %v", err)
	}
	rec, readErr := loadSeat(path)
	if tc.wantState == "" {
		if !errors.Is(readErr, os.ErrNotExist) {
			t.Fatalf("record %+v err %v", rec, readErr)
		}
	} else if readErr != nil || rec.State != tc.wantState {
		t.Fatalf("record %+v err %v", rec, readErr)
	}
	if tc.mode == "unknown" && bootouts != 0 {
		t.Fatal("unknown identity was booted out")
	}
	if strings.HasPrefix(tc.mode, "orphan-") && bootouts != 0 {
		t.Fatal("orphan was booted out")
	}
	if tc.mode == "bootstrap-failure" {
		if err == nil {
			t.Fatal("expected bootstrap error")
		}
		if !strings.Contains(err.Error(), "bootstrap") {
			t.Fatalf("expected bootstrap error, got %v", err)
		}
		if kills != 0 || strings.Contains(err.Error(), "kill-server") {
			t.Fatalf("unexpected cleanup kill: %v", err)
		}
	}
}

func reviewIdentify(mode string, booted *bool) func(int) (Identity, error) {
	return func(pid int) (Identity, error) {
		if mode == "agent-alive" && pid == 789 {
			return Identity{PID: pid, StartToken: "old"}, nil
		}
		if (mode == "dead" && !*booted) || mode == "kill-fails-stop" {
			return Identity{}, os.ErrNotExist
		}
		if strings.Contains(mode, "unknown") {
			return Identity{}, errors.New("unknown")
		}
		if mode == "invalid" {
			return Identity{PID: pid}, nil
		}
		token := "new"
		if mode == "alive-stop" {
			token = "old"
		}
		return Identity{PID: pid, StartToken: token}, nil
	}
}

func reviewRun(ctx context.Context, name string, args []string, mode string, present, booted *bool, bootouts, kills *int) ([]byte, error) {
	if name == "launchctl" {
		switch args[0] {
		case "print":
			if !*present {
				return []byte("localized"), exec.CommandContext(context.WithoutCancel(ctx), "sh", "-c", "exit 113").Run()
			}
			if *booted && mode == "timeout" {
				return []byte("state = waiting"), nil
			}
			return []byte("pid = 123\n"), nil
		case "bootout":
			*bootouts++
			*present = false
			return nil, nil
		case "bootstrap":
			if mode == "bootstrap-failure" {
				return nil, errors.New("bootstrap failed")
			}
			*booted = true
			*present = true
			return nil, nil
		}
	}
	if name == "tmux" && len(args) > 0 && args[len(args)-1] == "kill-server" {
		*kills++
	}
	if name == "tmux" && strings.HasPrefix(mode, "kill-fails") {
		return nil, errors.New("kill failed")
	}
	if strings.Contains(strings.Join(args, " "), "display") {
		return []byte("456"), nil
	}
	return nil, nil
}

func TestSeatSocketBoundary(t *testing.T) {
	for _, size := range []int{103, 104} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			home := shortTempDir(t)
			t.Setenv("HOME", home)
			dir := t.TempDir()
			name := strings.Repeat("x", size-len(filepath.Join(home, "agents/run/seats/"))-len("/.sock"))
			socket := filepath.Join(home, "agents/run/seats", name+".sock")
			if len(socket) != size {
				t.Fatalf("socket size %d", len(socket))
			}
			h := seatHost{dir: dir, timeout: time.Second, identify: func(int) (Identity, error) { return Identity{}, nil }, run: func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("reached launchctl") }}
			_, _, err := h.start(context.Background(), seatStartArgs{name: name, socket: socket})
			if err == nil {
				t.Fatal("expected error")
			}
			if size == 103 && !strings.Contains(err.Error(), "reached launchctl") {
				t.Fatal(err)
			}
			if size == 104 && !strings.Contains(err.Error(), "103 bytes") {
				t.Fatal(err)
			}
		})
	}
}

func TestShortTempDirFitsSocketCapUnderLongTMPDIR(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), strings.Repeat("deep", 40)))
	dir := shortTempDir(t)
	socket := filepath.Join(dir, "agents/run/seats", strings.Repeat("x", 20)+".sock")
	if len(socket) > 103 {
		t.Fatalf("socket path %d bytes, exceeds 103-byte cap: %s", len(socket), socket)
	}
}
