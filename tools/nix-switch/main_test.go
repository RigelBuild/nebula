// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// validConfig is a fixture with placeholder values.
const validConfig = `repo: "/repo"
flake: "example/nix"
hosts: ["example-host", "other-host"]
`

// exampleSwitch is the nh command the fixture config produces for example-host.
const exampleSwitch = "nh darwin switch /repo/example/nix#example-host --show-trace"

type fakeRunner struct {
	mu         sync.Mutex
	calls      []command
	fail       string
	failure    error
	empty      string
	conflicted bool
	keepalive  chan struct{}
}

func (f *fakeRunner) run(ctx context.Context, c command) error {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	key := c.name + " " + strings.Join(c.args, " ")
	if key == f.fail {
		return f.failure
	}
	if c.name == "jj" && len(c.args) > 0 && c.args[0] == "bookmark" && f.conflicted {
		_, err := fmt.Fprint(c.stdout, "main")
		return err
	}
	if c.name == "jj" && len(c.args) > 0 && c.args[0] == "log" {
		return f.log(c)
	}
	return f.waitKeepalive(ctx, key)
}

func (f *fakeRunner) log(c command) error {
	value := "abc123 description"
	switch c.args[len(c.args)-1] {
	case "empty":
		value = f.empty
	case "change_id.short()":
		value = "abc123"
	}
	_, err := fmt.Fprint(c.stdout, value)
	return err
}

func (f *fakeRunner) waitKeepalive(ctx context.Context, key string) error {
	if f.keepalive == nil {
		return nil
	}
	switch key {
	case "sudo -n -v":
		select {
		case f.keepalive <- struct{}{}:
		default:
		}
	case exampleSwitch:
		select {
		case <-f.keepalive:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
func (f *fakeRunner) snapshot() []command {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]command{}, f.calls...)
}

// writeConfig stores a config body in a temporary file and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.cue")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRun(t *testing.T) {
	exitErr := exec.Command("sh", "-c", "exit 17").Run()
	tests := []struct {
		name       string
		args       []string
		fail       string
		failure    error
		empty      string
		conflicted bool
		wantCode   int
		want       []string
		notice     string
	}{
		{name: "default", args: []string{"--repo", "/repo", "--", "--verbose"}, empty: "true", want: []string{"sudo -v", "jj log -r @ --no-graph -T empty", "jj git fetch", "jj bookmark list --conflicted main -T name", "jj new main", exampleSwitch + " --verbose"}},
		{name: "config repo", args: []string{}, empty: "true", want: []string{"sudo -v", "jj log -r @ --no-graph -T empty", "jj git fetch", "jj bookmark list --conflicted main -T name", "jj new main", exampleSwitch}},
		{name: "local", args: []string{"--repo", "/repo", "--local"}, want: []string{"sudo -v", "jj log -r @ --no-graph -T change_id.short() ++ \" \" ++ description.first_line()", exampleSwitch}, notice: "abc123 description"},
		{name: "sudo failure", args: []string{"--repo", "/repo"}, fail: "sudo -v", failure: errors.New("denied"), wantCode: 1, want: []string{"sudo -v"}},
		{name: "fetch failure", args: []string{"--repo", "/repo"}, empty: "true", fail: "jj git fetch", failure: errors.New("offline"), wantCode: 1, want: []string{"sudo -v", "jj log -r @ --no-graph -T empty", "jj git fetch"}},
		{name: "nh exit code", args: []string{"--repo", "/repo"}, empty: "true", fail: exampleSwitch, failure: exitErr, wantCode: 17, want: []string{"sudo -v", "jj log -r @ --no-graph -T empty", "jj git fetch", "jj bookmark list --conflicted main -T name", "jj new main", exampleSwitch}},
		{name: "nonempty", args: []string{"--repo", "/repo"}, empty: "false", wantCode: 1, want: []string{"sudo -v", "jj log -r @ --no-graph -T empty", "jj log -r @ --no-graph -T change_id.short()"}, notice: "working copy abc123 has uncommitted changes; run `nix-switch --local` to switch them, or commit/park them first"},
		{name: "conflicted main", args: []string{"--repo", "/repo"}, empty: "true", conflicted: true, wantCode: 1, want: []string{"sudo -v", "jj log -r @ --no-graph -T empty", "jj git fetch", "jj bookmark list --conflicted main -T name"}, notice: "main is conflicted"},
		{name: "local query failure", args: []string{"--repo", "/repo", "--local"}, fail: "jj log -r @ --no-graph -T change_id.short() ++ \" \" ++ description.first_line()", failure: errors.New("failed"), wantCode: 1, want: []string{"sudo -v", "jj log -r @ --no-graph -T change_id.short() ++ \" \" ++ description.first_line()"}},
		{name: "status query failure", args: []string{"--repo", "/repo"}, fail: "jj log -r @ --no-graph -T empty", failure: errors.New("failed"), wantCode: 1, want: []string{"sudo -v", "jj log -r @ --no-graph -T empty"}},
		{name: "change id failure", args: []string{"--repo", "/repo"}, empty: "false", fail: "jj log -r @ --no-graph -T change_id.short()", failure: errors.New("failed"), wantCode: 1, want: []string{"sudo -v", "jj log -r @ --no-graph -T empty", "jj log -r @ --no-graph -T change_id.short()"}},
		{name: "bad flag", args: []string{"--bad"}, wantCode: 2, want: []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeRunner{fail: tt.fail, failure: tt.failure, empty: tt.empty, conflicted: tt.conflicted}
			var out, stderr bytes.Buffer
			a := app{runner: f, stdin: strings.NewReader(""), stdout: &out, stderr: &stderr, interval: time.Hour}
			args := append([]string{"--config", writeConfig(t, validConfig), "--host", "example-host"}, tt.args...)
			if got := a.run(t.Context(), args); got != tt.wantCode {
				t.Fatalf("code %d, want %d; stderr: %s", got, tt.wantCode, stderr.String())
			}
			calls := f.snapshot()
			got := make([]string, 0, len(calls))
			for _, c := range calls {
				got = append(got, c.name+" "+strings.Join(c.args, " "))
				checkCommand(t, c)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("calls %q, want %q", got, tt.want)
			}
			if tt.notice != "" && !strings.Contains(out.String()+stderr.String(), tt.notice) {
				t.Errorf("output %q stderr %q lacks %q", out.String(), stderr.String(), tt.notice)
			}
		})
	}
}

func TestRequiredInputs(t *testing.T) {
	cfg := writeConfig(t, validConfig)
	tests := []struct {
		name     string
		args     []string
		wantCode int
		notice   string
	}{
		{name: "missing config", args: []string{"--host", "example-host"}, wantCode: 2, notice: "--config is required"},
		{name: "missing host", args: []string{"--config", cfg}, wantCode: 2, notice: "--host is required"},
		{name: "unlisted host", args: []string{"--config", cfg, "--host", "missing-host"}, wantCode: 2, notice: `host "missing-host" is not listed in the config`},
		{name: "absent config file", args: []string{"--config", filepath.Join(t.TempDir(), "absent.cue"), "--host", "example-host"}, wantCode: 1, notice: "load config: read config"},
		{name: "invalid config", args: []string{"--config", writeConfig(t, "hosts: []\n"), "--host", "example-host"}, wantCode: 1, notice: "load config: validate config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeRunner{}
			var out, stderr bytes.Buffer
			a := app{runner: f, stdin: strings.NewReader(""), stdout: &out, stderr: &stderr, interval: time.Hour}
			if got := a.run(t.Context(), tt.args); got != tt.wantCode {
				t.Fatalf("code %d, want %d; stderr: %s", got, tt.wantCode, stderr.String())
			}
			if calls := f.snapshot(); len(calls) != 0 {
				t.Errorf("ran %d commands before rejecting the input", len(calls))
			}
			if !strings.Contains(stderr.String(), tt.notice) {
				t.Errorf("stderr %q lacks %q", stderr.String(), tt.notice)
			}
		})
	}
}

func TestSwitchesSelectedHost(t *testing.T) {
	f := &fakeRunner{empty: "true"}
	var out, stderr bytes.Buffer
	a := app{runner: f, stdin: strings.NewReader(""), stdout: &out, stderr: &stderr, interval: time.Hour}
	args := []string{"--config", writeConfig(t, validConfig), "--host", "other-host", "--repo", "/repo"}
	if got := a.run(t.Context(), args); got != 0 {
		t.Fatalf("exit %d: %s", got, stderr.String())
	}
	calls := f.snapshot()
	last := calls[len(calls)-1]
	want := "nh darwin switch /repo/example/nix#other-host --show-trace"
	if got := last.name + " " + strings.Join(last.args, " "); got != want {
		t.Errorf("switch %q, want %q", got, want)
	}
}

func checkCommand(t *testing.T, c command) {
	t.Helper()
	if c.name == "jj" && c.dir != "/repo" {
		t.Errorf("jj dir %q", c.dir)
	}
	if c.name == "nh" && !reflect.DeepEqual(c.env, []string{"NH_SHOW_ACTIVATION_LOGS=1"}) {
		t.Errorf("nh env %v", c.env)
	}
}

func TestKeepaliveStops(t *testing.T) {
	f := &fakeRunner{empty: "true", keepalive: make(chan struct{}, 1)}
	var out, stderr bytes.Buffer
	a := app{runner: f, stdout: &out, stderr: &stderr, interval: time.Millisecond}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	args := []string{"--config", writeConfig(t, validConfig), "--host", "example-host", "--repo", "/repo"}
	if got := a.run(ctx, args); got != 0 {
		t.Fatalf("exit %d: %s", got, stderr.String())
	}
	calls := f.snapshot()
	found := false
	for _, c := range calls {
		if c.name == "sudo" && reflect.DeepEqual(c.args, []string{"-n", "-v"}) {
			found = true
		}
	}
	if !found {
		t.Fatal("keepalive did not run")
	}
}

// TestRepoFlagOverridesConfig gives --repo a value that differs from the config's repo.
// Every jj call and the nh flake path must use --repo.
func TestRepoFlagOverridesConfig(t *testing.T) {
	const cfgRepo = "/config-repo"
	cfgBody := "repo: \"" + cfgRepo + "\"\nflake: \"example/nix\"\nhosts: [\"example-host\"]\n"
	modes := []struct {
		name string
		args []string
	}{
		{name: "sync", args: nil},
		{name: "local", args: []string{"--local"}},
	}
	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			f := &fakeRunner{empty: "true"}
			var out, stderr bytes.Buffer
			a := app{runner: f, stdin: strings.NewReader(""), stdout: &out, stderr: &stderr, interval: time.Hour}
			args := append([]string{"--config", writeConfig(t, cfgBody), "--host", "example-host", "--repo", "/repo"}, mode.args...)
			if got := a.run(t.Context(), args); got != 0 {
				t.Fatalf("exit %d: %s", got, stderr.String())
			}
			var jjDirs []string
			var switchPath string
			for _, c := range f.snapshot() {
				switch c.name {
				case "jj":
					jjDirs = append(jjDirs, c.dir)
				case "nh":
					switchPath = c.args[2]
				}
			}
			if len(jjDirs) == 0 {
				t.Fatal("no jj command ran")
			}
			for _, dir := range jjDirs {
				if dir != "/repo" {
					t.Errorf("jj ran in %q, want --repo /repo", dir)
				}
			}
			if want := "/repo/example/nix#example-host"; switchPath != want {
				t.Errorf("nh flake %q, want %q", switchPath, want)
			}
		})
	}
}
