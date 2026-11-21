package main

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/RigelBuild/nebula/tools/fleet-switch/internal/terminal"
)

// exampleInventory is the caller-supplied inventory the tests run against.
const exampleInventory = "testdata/hosts.cue"

// testRepoURL is the trunk repository the tests pin against.
const testRepoURL = "https://git.example.com/example/repo"

func TestMain(m *testing.M) {
	repoURL = testRepoURL
	var err error
	fleetHosts, err = loadHosts(exampleInventory)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// openDevNull opens /dev/null for the test and reports a failed close.
func openDevNull(t *testing.T) *os.File {
	t.Helper()
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := stdin.Close(); err != nil {
			t.Errorf("close %s: %v", os.DevNull, err)
		}
	})
	return stdin
}

func TestSelectHosts(t *testing.T) {
	selected, skipped, err := selectHosts(nil, true, []string{"darwin1", "nixos4"})
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(selected))
	for _, h := range selected {
		names = append(names, h.name)
	}
	if !slices.Equal(names, []string{"nixos1", "nixos2", "nixos3", "nixos5", "wsl1"}) {
		t.Fatalf("selected: %v", names)
	}
	if len(skipped) != 2 {
		t.Fatalf("skipped: %v", skipped)
	}
	if _, _, err := selectHosts([]string{"nixos4"}, true, nil); err == nil {
		t.Fatal("all and explicit accepted")
	}
	if _, _, err := selectHosts([]string{"missing"}, false, nil); err == nil {
		t.Fatal("unknown host accepted")
	}
}

func TestWindowsHostIsExplicitOnly(t *testing.T) {
	all, _, err := selectHosts(nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(all, func(h host) bool { return h.name == "win1" }) {
		t.Fatal("--all selects win1")
	}
	for _, names := range [][]string{{"win1"}, {"wsl1", "win1"}} {
		selected, _, err := selectHosts(names, false, nil)
		if err != nil || len(selected) != len(names) {
			t.Fatalf("selected %v: %v", names, err)
		}
	}
}

func TestBootRebootConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ids     []string
		wantErr bool
	}{
		{name: "fast_reboot", ids: []string{"old", "new"}},
		{name: "slow_boot", ids: []string{"old", "old", "old", "new"}},
		{name: "no_reboot", ids: []string{"old"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			index := 0
			deps := newDependencies(io.Discard, io.Discard)
			deps.pollInterval = time.Millisecond
			deps.runner = func(ctx context.Context, name string, args []string, out, _ io.Writer) error {
				call := name + " " + strings.Join(args, " ")
				calls = append(calls, call)
				if strings.Contains(call, "boot_id") {
					id := tc.ids[min(index, len(tc.ids)-1)]
					index++
					_, err := io.WriteString(out, id+"\n")
					return err
				}
				if strings.Contains(call, "is-system-running") {
					_, err := io.WriteString(out, "running\n")
					return err
				}
				return nil
			}
			err := runBoot(t.Context(), deps, localRepo, []host{fleetHosts[4]}, false, false, 15*time.Millisecond)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error: %v; calls %v", err, calls)
			}
			if tc.wantErr && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expected timeout: %v", err)
			}
			if !tc.wantErr && !strings.Contains(calls[len(calls)-1], "is-system-running --wait") {
				t.Fatalf("health check missing: %v", calls)
			}
		})
	}
}

func TestBootTimeoutIncludesBuild(t *testing.T) {
	deps := newDependencies(io.Discard, io.Discard)
	deps.runner = func(ctx context.Context, _ string, _ []string, _, _ io.Writer) error {
		<-ctx.Done()
		return errors.New("process killed")
	}
	err := runBoot(t.Context(), deps, localRepo, []host{fleetHosts[4]}, false, false, time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected timeout: %v", err)
	}
}

func TestBootCancellationSkipsLaterHosts(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	deps := newDependencies(io.Discard, io.Discard)
	deps.runner = func(ctx context.Context, _ string, _ []string, _, _ io.Writer) error {
		calls++
		cancel()
		return ctx.Err()
	}
	err := runBoot(ctx, deps, localRepo, []host{fleetHosts[4], fleetHosts[5]}, false, false, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "remote state unknown") || calls != 1 {
		t.Fatalf("error %v, calls %d", err, calls)
	}
}

func TestSwitchCancellationSkipsLaterHosts(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	deps := newDependencies(io.Discard, io.Discard)
	deps.runner = func(ctx context.Context, _ string, _ []string, _, _ io.Writer) error {
		calls++
		cancel()
		return ctx.Err()
	}
	err := runSwitch(ctx, deps, localRepo, []host{fleetHosts[4], fleetHosts[5]}, false)
	if err == nil || !strings.Contains(err.Error(), "remote state unknown") || calls != 1 {
		t.Fatalf("error %v, calls %d", err, calls)
	}
}

func TestBootRefusesDarwin(t *testing.T) {
	calls := 0
	deps := newDependencies(io.Discard, io.Discard)
	deps.runner = func(context.Context, string, []string, io.Writer, io.Writer) error { calls++; return nil }
	err := runBoot(t.Context(), deps, localRepo, []host{fleetHosts[0]}, false, false, time.Minute)
	if err == nil || calls != 0 {
		t.Fatalf("error %v, calls %d", err, calls)
	}
}

func TestSwitchDryRun(t *testing.T) {
	var out strings.Builder
	deps := newDependencies(&out, io.Discard)
	if err := runSwitch(t.Context(), deps, localRepo, []host{fleetHosts[1]}, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "--target-host example-admin@nixos1.example.com --build-host example-admin@nixos1.example.com") {
		t.Fatal(out.String())
	}
}

func TestCommandSysProcAttr(t *testing.T) {
	for _, tc := range []struct {
		name     string
		terminal bool
		setpgid  bool
	}{
		{name: "terminal", terminal: true, setpgid: false},
		{name: "non-terminal", terminal: false, setpgid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attr := commandSysProcAttr(tc.terminal)
			if attr.Setpgid != tc.setpgid {
				t.Fatalf("Setpgid = %t, want %t", attr.Setpgid, tc.setpgid)
			}
		})
	}
}

func TestIsTerminalDevNull(t *testing.T) {
	if terminal.IsTerminal(openDevNull(t)) {
		t.Fatal("/dev/null is not a terminal")
	}
}

func TestRunCommandKillsProcessGroup(t *testing.T) {
	stdin := openDevNull(t)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	var out strings.Builder
	started := time.Now()
	err := runCommandWithStdin(ctx, "sh", []string{"-c", "sleep 30 & sleep 30"}, stdin, &out, io.Discard)
	if err == nil || time.Since(started) > 2*time.Second {
		t.Fatalf("error %v, elapsed %s", err, time.Since(started))
	}
}

func TestBootPollCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reads := 0
	deps := newDependencies(io.Discard, io.Discard)
	deps.runner = func(_ context.Context, name string, args []string, out, _ io.Writer) error {
		if name == "nix" && strings.Contains(strings.Join(args, " "), "#nixos5") {
			t.Fatal("later host started")
		}
		if strings.Contains(strings.Join(args, " "), "boot_id") {
			reads++
			if reads == 2 {
				cancel()
				return errors.New("interrupted")
			}
			_, err := io.WriteString(out, "old\n")
			return err
		}
		return nil
	}
	err := runBoot(ctx, deps, localRepo, []host{fleetHosts[4], fleetHosts[5]}, false, false, time.Second)
	if err == nil || !strings.Contains(err.Error(), "remote state unknown") || reads != 2 {
		t.Fatalf("error %v, reads %d", err, reads)
	}
}

func TestBootPollReadFailureTimesOut(t *testing.T) {
	reads := 0
	deps := newDependencies(io.Discard, io.Discard)
	deps.pollInterval = time.Millisecond
	deps.runner = func(_ context.Context, _ string, args []string, out, _ io.Writer) error {
		if strings.Contains(strings.Join(args, " "), "boot_id") {
			reads++
			if reads > 1 {
				return errors.New("offline")
			}
			_, err := io.WriteString(out, "old\n")
			return err
		}
		return nil
	}
	err := runBoot(t.Context(), deps, localRepo, []host{fleetHosts[4]}, false, false, 10*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || reads < 2 {
		t.Fatalf("error %v, reads %d", err, reads)
	}
}

// decodePowerShell reverses encodePowerShell, so tests can read the script a host runs.
func decodePowerShell(t *testing.T, encoded string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	units := make([]uint16, 0, len(raw)/2)
	for i := 0; i+1 < len(raw); i += 2 {
		units = append(units, binary.LittleEndian.Uint16(raw[i:]))
	}
	return string(utf16.Decode(units))
}

// mustGamingCommand returns h's gaming probe and fails the test if it cannot be built.
func mustGamingCommand(t *testing.T, h host) command {
	t.Helper()
	cmd, err := gamingCommand(h)
	if err != nil {
		t.Fatal(err)
	}
	return cmd
}

// mustHost returns the inventory host called name and fails the test if it is missing.
func mustHost(t *testing.T, name string) host {
	t.Helper()
	h, ok := findHost(name)
	if !ok {
		t.Fatalf("%s host missing", name)
	}
	return h
}

func TestEncodePowerShellKnownVector(t *testing.T) {
	// "a" is 0x61 0x00 in UTF-16LE, which base64 encodes as YQA=.
	if got := encodePowerShell("a"); got != "YQA=" {
		t.Fatalf("encodePowerShell(%q) = %q, want YQA=", "a", got)
	}
	// "ab" is 61 00 62 00 in UTF-16LE, which base64 encodes as YQBiAA==.
	if got := encodePowerShell("ab"); got != "YQBiAA==" {
		t.Fatalf("encodePowerShell(%q) = %q, want YQBiAA==", "ab", got)
	}
}

func TestWindowsSwitchCommands(t *testing.T) {
	withIdentityFileExists(t, false)
	windows := mustHost(t, "win1")
	cmds := windowsSwitchCommands(deploySource{rev: testRev}, windows)
	if len(cmds) != 2 {
		t.Fatalf("commands: %#v", cmds)
	}
	// The expected value is a literal, so this pins UTF-16LE without the test's decoder.
	if got := encodePowerShell("ab"); got != "YQBiAA==" {
		t.Fatalf("encoded vector drifted: %q", got)
	}
	prefix := slices.Concat(defaultSSHOptions, []string{"exampleuser@wsl1.example.com", "pwsh", "-NoProfile", "-NonInteractive", "-EncodedCommand"})
	for _, cmd := range cmds {
		if cmd.name != sshCommand || len(cmd.args) != len(prefix)+1 || !slices.Equal(cmd.args[:len(prefix)], prefix) {
			t.Fatalf("command: %#v", cmd)
		}
	}
	checkout := decodePowerShell(t, cmds[0].args[len(prefix)])
	last := -1
	for _, want := range []string{"git status --porcelain", "if ($dirty) { throw", "$env:GIT_TERMINAL_PROMPT = '0'", "$env:GCM_INTERACTIVE = 'Never'", "git fetch origin " + testRev, "git checkout --detach " + testRev} {
		i := strings.Index(checkout, want)
		if i <= last {
			t.Fatalf("%q missing or out of order in checkout script:\n%s", want, checkout)
		}
		last = i
	}
	if strings.Contains(checkout, "reset") || strings.Contains(checkout, "clean") {
		t.Fatalf("checkout script discards work:\n%s", checkout)
	}
	setup := decodePowerShell(t, cmds[1].args[len(prefix)])
	const wantSetup = `pwsh -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "$env:USERPROFILE\repos\example\setup\windows-setup.ps1" -NonInteractive`
	if !strings.Contains(setup, wantSetup) {
		t.Fatalf("setup script:\n%s", setup)
	}
}

func TestWindowsDryRunPrintsCommandsWithoutRunning(t *testing.T) {
	withIdentityFileExists(t, false)
	windows := mustHost(t, "win1")
	cmds := windowsSwitchCommands(deploySource{rev: testRev}, windows)
	calls := 0
	var out strings.Builder
	deps := newDependencies(&out, io.Discard)
	deps.runner = func(context.Context, string, []string, io.Writer, io.Writer) error { calls++; return nil }
	if err := runSwitch(t.Context(), deps, deploySource{rev: testRev}, []host{windows}, true); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("dry run executed %d commands", calls)
	}
	for _, line := range []string{formatCommand(mustGamingCommand(t, windows)), formatCommand(cmds[0]), formatCommand(cmds[1])} {
		if !strings.Contains(out.String(), line+"\n") {
			t.Fatalf("missing %q in:\n%s", line, out.String())
		}
	}
	for _, want := range []string{"git checkout --detach " + testRev, "if ($dirty) { throw", `windows-setup.ps1" -NonInteractive`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in dry run:\n%s", want, out.String())
		}
	}
}

func TestWindowsGamingProbe(t *testing.T) {
	withIdentityFileExists(t, false)
	windows := mustHost(t, "win1")
	probeArgs := slices.Concat(defaultSSHOptions, []string{"-p", "2222", "exampleuser@wsl1.example.com", "test", "-e", windows.gamingFlag})
	for _, tc := range []struct {
		name      string
		code      int
		wantCalls int
		failed    bool
	}{
		{name: "gaming", code: 0, wantCalls: 1},
		{name: "not_gaming", code: 1, wantCalls: 3},
		{name: "ssh_error", code: 255, wantCalls: 1, failed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			var out strings.Builder
			deps := newDependencies(&out, io.Discard)
			deps.runner = func(ctx context.Context, name string, args []string, _, _ io.Writer) error {
				calls++
				if calls == 1 {
					if name != sshCommand || !slices.Equal(args, probeArgs) {
						t.Fatalf("probe: %s %v", name, args)
					}
					if tc.code != 0 {
						return exec.CommandContext(ctx, "sh", "-c", fmt.Sprintf("exit %d", tc.code)).Run()
					}
				}
				return nil
			}
			err := runSwitch(t.Context(), deps, deploySource{rev: testRev}, []host{windows}, false)
			if (err != nil) != tc.failed || calls != tc.wantCalls {
				t.Fatalf("error %v, calls %d\n%s", err, calls, out.String())
			}
			if tc.code == 0 && !strings.Contains(out.String(), "win1: gaming mode on, skipped") {
				t.Fatal(out.String())
			}
		})
	}
}

func TestWindowsSwitchStopsAfterFailedCheckout(t *testing.T) {
	withIdentityFileExists(t, false)
	calls := 0
	deps := newDependencies(io.Discard, io.Discard)
	deps.runner = func(ctx context.Context, _ string, _ []string, _, _ io.Writer) error {
		calls++
		switch calls {
		case 1: // the gaming probe: exit 1 means the probe host is not gaming
			return exec.CommandContext(ctx, "sh", "-c", "exit 1").Run()
		case 2: // the checkout fails
			return errors.New("checkout failed")
		}
		t.Fatalf("call %d ran after the checkout failed", calls)
		return nil
	}
	err := runSwitch(t.Context(), deps, deploySource{rev: testRev}, []host{mustHost(t, "win1")}, false)
	if err == nil || !strings.Contains(err.Error(), "win1") || calls != 2 {
		t.Fatalf("error %v, calls %d", err, calls)
	}
}

