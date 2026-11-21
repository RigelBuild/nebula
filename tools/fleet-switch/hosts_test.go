package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestExampleHosts(t *testing.T) {
	hosts, err := loadHosts(exampleInventory)
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	want := []host{
		{name: "darwin1", target: "darwin1.example.com", platform: "darwin", flake: "example/nix#darwin1", user: "root", port: 22},
		{name: "nixos1", target: "nixos1.example.com", platform: "nixos", flake: "example/nix#nixos1", user: "root", port: 22, rebootable: true},
		{name: "nixos2", target: "nixos2.example.com", platform: "nixos", flake: "example/nix#nixos2", user: "root", port: 22, rebootable: true},
		{name: "nixos3", target: "nixos3.example.com", platform: "nixos", flake: "example/nix#nixos3", user: "root", port: 22, rebootable: true},
		{name: "nixos4", target: "nixos4.example.com", platform: "nixos", flake: "example/nix#nixos4", user: "root", port: 22, rebootable: true},
		{name: "nixos5", target: "nixos5.example.com", platform: "nixos", flake: "example/nix#nixos5", user: "root", port: 22, rebootable: true},
		{name: "wsl1", target: "wsl1.example.com", platform: "wsl", flake: "example/wsl#wsl1", user: "exampleuser", port: 2222, identityFile: filepath.Join(home, ".ssh/id_example"), gamingFlag: "/var/lib/example-mode/gaming", localCheckout: true},
		{name: "win1", target: "wsl1.example.com", platform: "windows", user: "exampleuser", port: 22, gamingFlag: "/var/lib/example-mode/gaming", gamingProbeHost: "wsl1", explicitOnly: true, windowsCheckout: "repos\\example", windowsSetup: "setup\\windows-setup.ps1"},
	}
	if !reflect.DeepEqual(hosts, want) {
		t.Fatalf("hosts: %#v", hosts)
	}
}

func TestLoadHostsRequiresInventory(t *testing.T) {
	if _, err := loadHosts(""); err == nil || !strings.Contains(err.Error(), "no hosts inventory") {
		t.Fatalf("error: %v", err)
	}
}

func TestInvalidHosts(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"platform", `hosts: [{name: "x", target: "x", platform: "bad", flake: "x#x", rebootable: true}]`, "platform"},
		{"missing_target", `hosts: [{name: "x", platform: "nixos", flake: "x#x", rebootable: true}]`, "target"},
		{"empty_name", `hosts: [{name: "", target: "x", platform: "nixos", flake: "x#x", rebootable: true}]`, "name"},
		{"empty_target", `hosts: [{name: "x", target: "", platform: "nixos", flake: "x#x", rebootable: true}]`, "target"},
		{"empty_flake", `hosts: [{name: "x", target: "x", platform: "nixos", flake: "", rebootable: true}]`, "flake"},
		{"port_zero", `hosts: [{name: "x", target: "x", platform: "nixos", flake: "x#x", rebootable: true, port: 0}]`, "port"},
		{"port_high", `hosts: [{name: "x", target: "x", platform: "nixos", flake: "x#x", rebootable: true, port: 65536}]`, "port"},
		{"duplicate", `hosts: [{name: "x", target: "x", platform: "nixos", flake: "x#x", rebootable: true}, {name: "x", target: "y", platform: "nixos", flake: "x#y", rebootable: true}]`, "duplicate"},
		{"missing_flake", `hosts: [{name: "x", target: "x", platform: "nixos", rebootable: true}]`, "flake"},
		{"empty_gaming_probe_host", `hosts: [{name: "x", target: "x", platform: "windows", rebootable: false, windowsCheckout: "c", windowsSetup: "s", gamingProbeHost: ""}]`, "gamingProbeHost"},
		{"unknown_gaming_probe_host", `hosts: [{name: "x", target: "x", platform: "windows", rebootable: false, windowsCheckout: "c", windowsSetup: "s", gamingProbeHost: "nope"}]`, "not a host in this inventory"},
		{"self_gaming_probe_host", `hosts: [{name: "x", target: "x", platform: "nixos", flake: "x#x", rebootable: true, gamingProbeHost: "x"}]`, "names the host itself"},
		{"windows_gaming_probe_host", `hosts: [{name: "w", target: "w", platform: "windows", rebootable: false, windowsCheckout: "c", windowsSetup: "s"}, {name: "x", target: "x", platform: "windows", rebootable: false, windowsCheckout: "c", windowsSetup: "s", gamingProbeHost: "w"}]`, "is a windows host"},
		{"windows_rebootable", `hosts: [{name: "x", target: "x", platform: "windows", rebootable: true, windowsCheckout: "c", windowsSetup: "s"}]`, "conflicting values"},
		{"windows_missing_checkout", `hosts: [{name: "x", target: "x", platform: "windows", rebootable: false, windowsSetup: "s"}]`, "windowsCheckout"},
		{"windows_missing_setup", `hosts: [{name: "x", target: "x", platform: "windows", rebootable: false, windowsCheckout: "c"}]`, "windowsSetup"},
		{"windows_checkout_quote", `hosts: [{name: "x", target: "x", platform: "windows", rebootable: false, windowsCheckout: "c'x", windowsSetup: "s"}]`, "windowsCheckout"},
		{"windows_checkout_double_quote", `hosts: [{name: "x", target: "x", platform: "windows", rebootable: false, windowsCheckout: "c\"x", windowsSetup: "s"}]`, "windowsCheckout"},
		{"windows_setup_dollar", `hosts: [{name: "x", target: "x", platform: "windows", rebootable: false, windowsCheckout: "c", windowsSetup: "s$x"}]`, "windowsSetup"},
		{"windows_setup_backtick", `hosts: [{name: "x", target: "x", platform: "windows", rebootable: false, windowsCheckout: "c", windowsSetup: "s\u0060x"}]`, "windowsSetup"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hosts.cue")
			if err := os.WriteFile(path, []byte(tc.input), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := loadHosts(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error: %v", err)
			}
		})
	}
}

func TestWSLCommand(t *testing.T) {
	withIdentityFileExists(t, true)
	h := mustHost(t, "wsl1")
	cmd := nixosRebuildCommand("switch", localRepo, h)
	want := []string{"NIX_SSHOPTS=-p 2222 -i " + h.identityFile + " -o IdentitiesOnly=yes", "nix", "run", "--inputs-from", "/repo/example/wsl", "nixpkgs#nixos-rebuild", "--", "switch", "--flake", "/repo/example/wsl#wsl1", "--target-host", "exampleuser@wsl1.example.com", "--build-host", "exampleuser@wsl1.example.com", "--show-trace", "--sudo"}
	if cmd.name != "env" || !reflect.DeepEqual(cmd.args, want) {
		t.Fatalf("command: %#v", cmd)
	}
}

// withIdentityFileExists pins the identity-file existence check for one test.
func withIdentityFileExists(t *testing.T, exists bool) {
	t.Helper()
	prev := identityFileExists
	identityFileExists = func(string) bool { return exists }
	t.Cleanup(func() { identityFileExists = prev })
}

func TestWSLIdentityFileOptions(t *testing.T) {
	wsl, ok := findHost("wsl1")
	if !ok {
		t.Fatal("wsl1 host missing")
	}
	tests := []struct {
		name     string
		exists   bool
		wantOpts []string
	}{
		{name: "key_missing", exists: false, wantOpts: []string{"-p", "2222"}},
		{name: "key_present", exists: true, wantOpts: []string{"-p", "2222", "-i", wsl.identityFile, "-o", "IdentitiesOnly=yes"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withIdentityFileExists(t, tc.exists)
			if got := sshOptions(wsl); !slices.Equal(got, tc.wantOpts) {
				t.Fatalf("sshOptions: %v", got)
			}
			wantProbe := slices.Concat(defaultSSHOptions, tc.wantOpts, []string{"exampleuser@" + wsl.target, "test", "-e", wsl.gamingFlag})
			probe, err := gamingCommand(wsl)
			if err != nil || probe.name != sshCommand || !slices.Equal(probe.args, wantProbe) {
				t.Fatalf("gaming probe: %#v, %v", probe, err)
			}
			cmd := nixosRebuildCommand("switch", localRepo, wsl)
			wantEnv := "NIX_SSHOPTS=" + strings.Join(tc.wantOpts, " ")
			if cmd.name != "env" || cmd.args[0] != wantEnv {
				t.Fatalf("rebuild: %#v", cmd)
			}
		})
	}
}

func TestHostWithoutIdentityFileOmitsIdentityOptions(t *testing.T) {
	withIdentityFileExists(t, true)
	plain := host{name: "plain", target: "plain.example", user: "root", port: 22}
	if got := sshOptions(plain); len(got) != 0 {
		t.Fatalf("sshOptions: %v", got)
	}
	cmd := hostSSHCommand(plain, "true")
	want := []string{"-o", "ConnectTimeout=10", "root@plain.example", "true"}
	if cmd.name != sshCommand || !slices.Equal(cmd.args, want) {
		t.Fatalf("command: %#v", cmd)
	}
}

type gamingProbeCase struct {
	name      string
	code      int
	wantCalls int
	failed    bool
}

func TestGamingProbe(t *testing.T) {
	tests := []gamingProbeCase{
		{name: "gaming", code: 0, wantCalls: 1},
		{name: "not_gaming", code: 1, wantCalls: 2},
		{name: "ssh_error", code: 255, wantCalls: 1, failed: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			testGamingProbeCase(t, tc)
		})
	}
}

func testGamingProbeCase(t *testing.T, tc gamingProbeCase) {
	t.Helper()
	calls := 0
	var out strings.Builder
	deps := newDependencies(&out, io.Discard)
	deps.runner = func(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error {
		calls++
		if calls == 1 && !slices.Contains(args, "ConnectTimeout=10") {
			t.Errorf("probe missing timeout: %v", args)
		}
		if calls == 1 && tc.code != 0 {
			return exec.CommandContext(ctx, "sh", "-c", fmt.Sprintf("exit %d", tc.code)).Run()
		}
		return nil
	}
	err := runSwitch(t.Context(), deps, localRepo, []host{mustHost(t, "wsl1")}, false)
	if (err != nil) != tc.failed || calls != tc.wantCalls {
		t.Fatalf("error %v, calls %d", err, calls)
	}
	if tc.code == 0 {
		assertGamingProbeOutput(t, out.String())
	}
}

func assertGamingProbeOutput(t *testing.T, output string) {
	t.Helper()
	if !strings.Contains(output, "wsl1: gaming mode on, skipped") {
		t.Fatal(output)
	}
	if !strings.Contains(output, "✓ switched 0, skipped 1 (gaming)") {
		t.Fatal(output)
	}
}

func TestGamingProbeInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	deps := newDependencies(io.Discard, io.Discard)
	deps.runner = func(context.Context, string, []string, io.Writer, io.Writer) error {
		cancel()
		return context.Canceled
	}
	err := runSwitch(ctx, deps, localRepo, []host{mustHost(t, "wsl1")}, false)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "switch interrupted during gaming probe on wsl1; no changes made") {
		t.Fatalf("error: %v", err)
	}
}

func TestDarwinSwitchCommandRunsFromStorePath(t *testing.T) {
	cmd := darwinSwitchCommand(darwinStoreRef("/nix/store/0123456789abcdfghijklmnpqrsvwxyz-src", fleetHosts[0]), fleetHosts[0])
	want := []string{"-t", "darwin1", `zsh -lic 'nh-switch darwin "path:/nix/store/0123456789abcdfghijklmnpqrsvwxyz-src?dir=example/nix#darwin1"'`}
	if cmd.name != "ssh" || !slices.Equal(cmd.args, want) {
		t.Fatalf("command: %#v", cmd)
	}
}

func TestWSLDryRunAndBoot(t *testing.T) {
	calls := 0
	var out strings.Builder
	deps := newDependencies(&out, io.Discard)
	deps.runner = func(context.Context, string, []string, io.Writer, io.Writer) error { calls++; return nil }
	if err := runSwitch(t.Context(), deps, localRepo, []host{mustHost(t, "wsl1")}, true); err != nil {
		t.Fatal(err)
	}
	if calls != 0 || !strings.Contains(out.String(), "test -e /var/lib/example-mode/gaming") || !strings.Contains(out.String(), "--sudo") {
		t.Fatal(out.String())
	}
	if err := runBoot(t.Context(), deps, localRepo, []host{mustHost(t, "wsl1")}, false, false, time.Minute); err == nil || calls != 0 {
		t.Fatalf("error %v, calls %d", err, calls)
	}
}
