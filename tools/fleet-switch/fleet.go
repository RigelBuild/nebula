package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec" //nolint:depguard // fleet-switch invokes fixed system tools without a shell.
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"

	"github.com/RigelBuild/nebula/tools/fleet-switch/internal/terminal"
)

const (
	platformDarwin  = "darwin"
	platformNixOS   = "nixos"
	platformWindows = "windows"
	sshCommand      = "ssh"
)

var (
	fleetHosts []host
	// repoURL is the trunk repository; the --repo-url flag sets it.
	repoURL string
)

type command struct {
	name string
	args []string
}

type commandRunner func(context.Context, string, []string, io.Writer, io.Writer) error

type dependencies struct {
	skippedHosts []host
	runner       commandRunner
	stdout       io.Writer
	stderr       io.Writer
	sleep        func(context.Context, time.Duration) error
	pollInterval time.Duration
}

func newDependencies(stdout, stderr io.Writer) dependencies {
	return dependencies{
		runner:       runCommand,
		stdout:       stdout,
		stderr:       stderr,
		sleep:        sleepContext,
		pollInterval: 5 * time.Second,
	}
}

func commandSysProcAttr(terminal bool) *syscall.SysProcAttr {
	// Keep interactive children in the foreground group so they can read the terminal.
	return &syscall.SysProcAttr{Setpgid: !terminal}
}

func runCommand(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error {
	return runCommandWithStdin(ctx, name, args, os.Stdin, stdout, stderr)
}

func runCommandWithStdin(ctx context.Context, name string, args []string, stdin *os.File, stdout, stderr io.Writer) error {
	process := exec.CommandContext(ctx, name, args...) //nolint:gosec // Command names and arguments come from fixed builders.
	process.SysProcAttr = commandSysProcAttr(terminal.IsTerminal(stdin))
	if process.SysProcAttr.Setpgid {
		process.Cancel = func() error {
			err := syscall.Kill(-process.Process.Pid, syscall.SIGKILL)
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
	}
	process.WaitDelay = 3 * time.Second
	process.Stdin = stdin
	process.Stdout = stdout
	process.Stderr = stderr
	return process.Run()
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func knownHostNames() string {
	names := make([]string, 0, len(fleetHosts))
	for _, fleetHost := range fleetHosts {
		names = append(names, fleetHost.name)
	}
	return strings.Join(names, ", ")
}

func selectHosts(explicit []string, all bool, except []string) ([]host, []host, error) {
	if all && len(explicit) > 0 {
		return nil, nil, errors.New("--all cannot be combined with explicit hosts")
	}

	excepted := make(map[string]struct{}, len(except))
	for _, name := range except {
		if _, ok := findHost(name); !ok {
			return nil, nil, fmt.Errorf("--except: unknown host %q (known: %s)", name, knownHostNames())
		}
		excepted[name] = struct{}{}
	}

	selected := make([]host, 0, len(fleetHosts))
	if all {
		// explicitOnly hosts run only when named.
		for _, fleetHost := range fleetHosts {
			if !fleetHost.explicitOnly {
				selected = append(selected, fleetHost)
			}
		}
	} else {
		if len(explicit) == 0 {
			return nil, nil, fmt.Errorf("no hosts selected; specify hosts or --all (known: %s)", knownHostNames())
		}
		for _, name := range explicit {
			fleetHost, ok := findHost(name)
			if !ok {
				return nil, nil, fmt.Errorf("unknown host %q (known: %s)", name, knownHostNames())
			}
			selected = append(selected, fleetHost)
		}
	}

	filtered := make([]host, 0, len(selected))
	skipped := make([]host, 0, len(selected))
	for _, fleetHost := range selected {
		if _, skip := excepted[fleetHost.name]; skip {
			skipped = append(skipped, fleetHost)
			continue
		}
		filtered = append(filtered, fleetHost)
	}
	if len(filtered) == 0 {
		return nil, nil, errors.New("--except leaves no host selected")
	}
	return filtered, skipped, nil
}

func findHost(name string) (host, bool) {
	for _, fleetHost := range fleetHosts {
		if fleetHost.name == name {
			return fleetHost, true
		}
	}
	return host{}, false
}

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// deploySource is one run's flake origin, fixed before the first host is touched.
type deploySource struct {
	root string // repository root for --flake PATH; empty when deploying trunk
	rev  string // trunk commit pinned for the run; empty for a local checkout
}

func (src deploySource) isLocal() bool { return src.root != "" }

// treeRef is the pinned trunk tree; flakeRef and the darwin fetch both start from it.
func (src deploySource) treeRef() string {
	return "git+" + repoURL + "?ref=main&rev=" + src.rev
}

// flakeRef is host's flake. Trunk pins the commit and selects the host's subdirectory.
func (src deploySource) flakeRef(fleetHost host) string {
	dir, attr, _ := strings.Cut(fleetHost.flake, "#")
	if src.isLocal() {
		return filepath.Join(src.root, dir) + "#" + attr
	}
	return src.treeRef() + "&dir=" + dir + "#" + attr
}

// resolveSource returns the --flake checkout, or pins trunk's main once for the whole run.
// flakeSet reports that --flake was given, so an explicit empty value is an error.
func resolveSource(ctx context.Context, deps dependencies, flake string, flakeSet bool) (deploySource, error) {
	if flakeSet {
		if flake == "" {
			return deploySource{}, errors.New("--flake PATH is empty; give a repository root or omit --flake to deploy trunk")
		}
		root, err := resolveFlake(flake, ".")
		return deploySource{root: root}, err
	}
	rev, err := trunkRev(ctx, deps)
	if err != nil {
		return deploySource{}, err
	}
	return deploySource{rev: rev}, nil
}

// trunkRev asks origin for main's head; ls-remote prints "<sha>\trefs/heads/main".
func trunkRev(ctx context.Context, deps dependencies) (string, error) {
	if repoURL == "" {
		return "", errors.New("--repo-url is required to deploy trunk")
	}
	var stdout bytes.Buffer
	args := []string{"ls-remote", repoURL, "refs/heads/main"}
	if err := deps.runner(ctx, "git", args, &stdout, deps.stderr); err != nil {
		return "", fmt.Errorf("resolve %s main: %w", repoURL, err)
	}
	fields := strings.Fields(stdout.String())
	if len(fields) != 2 || fields[1] != "refs/heads/main" || !fullSHA.MatchString(fields[0]) {
		return "", fmt.Errorf("resolve %s main: unexpected ls-remote output %q", repoURL, stdout.String())
	}
	return fields[0], nil
}

// resolveFlake returns the absolute repository root named by --flake PATH.
func resolveFlake(flake, cwd string) (string, error) {
	if filepath.IsAbs(flake) {
		return filepath.Clean(flake), nil
	}
	path, err := filepath.Abs(filepath.Join(cwd, flake))
	if err != nil {
		return "", fmt.Errorf("resolve flake path: %w", err)
	}
	return path, nil
}

// checkSource refuses a local-checkout deploy before any host is touched.
func checkSource(selected []host, src deploySource, allowLocal bool) error {
	if !src.isLocal() {
		return nil
	}
	if !allowLocal {
		for _, fleetHost := range selected {
			if !fleetHost.localCheckout {
				return fmt.Errorf("--flake PATH deploys a local checkout, which only hosts with localCheckout may use; pass --allow-local to deploy %s from it", fleetHost.name)
			}
		}
	}
	for _, fleetHost := range selected {
		if fleetHost.platform == platformDarwin || fleetHost.platform == platformWindows {
			return fmt.Errorf("%s cannot deploy a local checkout; deploy trunk or skip it with --except %s", fleetHost.name, fleetHost.name)
		}
	}
	return nil
}

func nixosRebuildCommand(action string, src deploySource, fleetHost host) command {
	flake := src.flakeRef(fleetHost)
	args := []string{"run", "--inputs-from", strings.SplitN(flake, "#", 2)[0], "nixpkgs#nixos-rebuild", "--",
		action, "--flake", flake, "--target-host", fleetHost.user + "@" + fleetHost.target,
		"--build-host", fleetHost.user + "@" + fleetHost.target, "--show-trace"}
	if fleetHost.user != "root" {
		args = append(args, "--sudo")
	}
	options := sshOptions(fleetHost)
	if len(options) != 0 {
		return command{name: "env", args: append([]string{"NIX_SSHOPTS=" + strings.Join(options, " "), "nix"}, args...)}
	}
	return command{name: "nix", args: args}
}

// windowsSetupScript runs the host's setup script from its checkout, unattended.
func windowsSetupScript(h host) string {
	return `pwsh -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "$env:USERPROFILE\` + h.windowsCheckout + `\` + h.windowsSetup + `" -NonInteractive
exit $LASTEXITCODE`
}

// windowsCheckoutScript moves the host's Windows checkout to sha. A dirty tree throws before any git write.
func windowsCheckoutScript(h host, sha string) string {
	return `$ErrorActionPreference = 'Stop'
$checkout = Join-Path $env:USERPROFILE '` + h.windowsCheckout + `'
Set-Location -LiteralPath $checkout
$dirty = git status --porcelain
if ($LASTEXITCODE -ne 0) { throw "git status failed in $checkout" }
if ($dirty) { throw "$checkout has local changes; commit or stash them, then rerun fleet-switch" }
$env:GIT_TERMINAL_PROMPT = '0'
$env:GCM_INTERACTIVE = 'Never'
git fetch origin ` + sha + `
if ($LASTEXITCODE -ne 0) { throw "git fetch failed in $checkout; Windows git needs a credential the ssh session can read (for example, gh auth login in that session)" }
git checkout --detach ` + sha + `
if ($LASTEXITCODE -ne 0) { throw "git checkout failed in $checkout" }`
}

// encodePowerShell returns script as base64 UTF-16LE, the form pwsh -EncodedCommand reads.
func encodePowerShell(script string) string {
	units := utf16.Encode([]rune(script))
	raw := make([]byte, 0, 2*len(units))
	for _, unit := range units {
		raw = binary.LittleEndian.AppendUint16(raw, unit)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// powershellCommand runs script under pwsh on h; encoding keeps $ and quotes away from cmd and pwsh.
func powershellCommand(h host, script string) command {
	return hostSSHCommand(h, "pwsh", "-NoProfile", "-NonInteractive", "-EncodedCommand", encodePowerShell(script))
}

// windowsSwitchScripts are the PowerShell steps that move a Windows host to src's commit, then run its setup.
// checkSource refuses local checkouts for Windows hosts, so src.rev is the pinned trunk commit.
func windowsSwitchScripts(src deploySource, h host) []string {
	return []string{windowsCheckoutScript(h, src.rev), windowsSetupScript(h)}
}

// windowsSwitchCommands wraps each windowsSwitchScripts step in one encoded pwsh call over ssh.
func windowsSwitchCommands(src deploySource, h host) []command {
	scripts := windowsSwitchScripts(src, h)
	cmds := make([]command, 0, len(scripts))
	for _, script := range scripts {
		cmds = append(cmds, powershellCommand(h, script))
	}
	return cmds
}

// identityFileExists reports whether an identity file is on disk; tests replace it.
var identityFileExists = func(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func sshOptions(h host) []string {
	options := []string{}
	if h.port != 22 {
		options = append(options, "-p", strconv.Itoa(h.port))
	}
	// Omit the key when absent so ssh falls back to its config and agent.
	if h.identityFile != "" && identityFileExists(h.identityFile) {
		options = append(options, "-i", h.identityFile, "-o", "IdentitiesOnly=yes")
	}
	return options
}

// gamingCommand probes h's gaming flag over ssh; gamingProbeHost, when set, is probed instead.
func gamingCommand(h host) (command, error) {
	probe := h
	if h.gamingProbeHost != "" {
		var ok bool
		probe, ok = findHost(h.gamingProbeHost)
		if !ok {
			return command{}, fmt.Errorf("gaming probe host %q for %s is unknown", h.gamingProbeHost, h.name)
		}
	}
	return hostSSHCommand(probe, "test", "-e", shellQuote(h.gamingFlag)), nil
}

func gamingSkip(ctx context.Context, deps dependencies, h host, dryRun bool) (bool, error) {
	if h.gamingFlag == "" {
		return false, nil
	}
	cmd, err := gamingCommand(h)
	if err != nil {
		return false, err
	}
	if dryRun {
		_, err := fmt.Fprintln(deps.stdout, formatCommand(cmd))
		return false, err
	}
	err = deps.runner(ctx, cmd.name, cmd.args, deps.stdout, deps.stderr)
	if err == nil {
		_, err = fmt.Fprintf(deps.stdout, "%s: gaming mode on, skipped\n", h.name)
		return true, err
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("gaming probe on %s: %w", h.name, err)
}

func formatCommand(cmd command) string {
	parts := make([]string, 0, len(cmd.args)+1)
	parts = append(parts, shellQuote(cmd.name))
	for _, arg := range cmd.args {
		parts = append(parts, shellQuote(arg))
	}
	return strings.Join(parts, " ")
}

func shellQuote(value string) string {
	if value != "" && strings.IndexFunc(value, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune("_@%+=:,./#-", r)
	}) == -1 {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func bootHost(ctx context.Context, deps dependencies, fleetHost host, build command, noReboot bool, timeout time.Duration) error {
	hostCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := deps.runner(hostCtx, build.name, build.args, deps.stdout, deps.stderr); err != nil {
		if hostCtx.Err() != nil {
			return fmt.Errorf("boot failed on %s: %w", fleetHost.name, hostCtx.Err())
		}
		return fmt.Errorf("boot failed on %s: %w", fleetHost.name, err)
	}
	if noReboot {
		_, err := fmt.Fprintf(deps.stdout, "✓ %s boot configuration installed; reboot skipped.\n", fleetHost.name)
		return err
	}
	oldID, err := readBootID(hostCtx, deps, fleetHost)
	if err != nil {
		return fmt.Errorf("read boot ID on %s: %w", fleetHost.name, err)
	}
	reboot := rebootCommand(fleetHost)
	if err := deps.runner(hostCtx, reboot.name, reboot.args, deps.stdout, deps.stderr); err != nil {
		var exitErr *exec.ExitError
		if hostCtx.Err() != nil {
			return fmt.Errorf("reboot request on %s: %w", fleetHost.name, hostCtx.Err())
		}
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 255 {
			return fmt.Errorf("reboot request failed on %s: %w", fleetHost.name, err)
		}
	}
	if err := waitForHost(hostCtx, deps, fleetHost, oldID); err != nil {
		return err
	}
	state, err := checkSystemState(hostCtx, deps, fleetHost)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(deps.stdout, "✓ %s returned (%s).\n", fleetHost.name, state)
	return err
}

// defaultSSHOptions bound connection setup; prompts after connecting can still block.
var defaultSSHOptions = []string{"-o", "ConnectTimeout=10"}

func hostSSHCommand(h host, args ...string) command {
	options := append(append([]string{}, defaultSSHOptions...), sshOptions(h)...)
	options = append(options, h.user+"@"+h.target)
	return command{name: sshCommand, args: append(options, args...)}
}

func rebootCommand(h host) command {
	if h.user != "root" {
		return hostSSHCommand(h, "sudo", "systemctl", "reboot")
	}
	return hostSSHCommand(h, "systemctl", "reboot")
}

func bootIDCommand(h host) command {
	return hostSSHCommand(h, "cat", "/proc/sys/kernel/random/boot_id")
}

func healthCommand(h host) command {
	return hostSSHCommand(h, "systemctl", "is-system-running", "--wait")
}

func readBootID(ctx context.Context, deps dependencies, fleetHost host) (string, error) {
	cmd := bootIDCommand(fleetHost)
	var stdout bytes.Buffer
	if err := deps.runner(ctx, cmd.name, cmd.args, &stdout, deps.stderr); err != nil {
		return "", err
	}
	id := strings.TrimSpace(stdout.String())
	if id == "" {
		return "", errors.New("empty boot ID")
	}
	return id, nil
}

func waitForHost(ctx context.Context, deps dependencies, fleetHost host, oldID string) error {
	for {
		id, err := readBootID(ctx, deps, fleetHost)
		if err == nil && id != oldID {
			return nil
		}
		if err := deps.sleep(ctx, deps.pollInterval); err != nil {
			return fmt.Errorf("waiting for changed boot ID on %s: %w", fleetHost.name, err)
		}
	}
}

func checkSystemState(ctx context.Context, deps dependencies, fleetHost host) (string, error) {
	cmd := healthCommand(fleetHost)
	var stdout bytes.Buffer
	err := deps.runner(ctx, cmd.name, cmd.args, io.MultiWriter(&stdout, deps.stdout), deps.stderr)
	if ctx.Err() != nil {
		return "", fmt.Errorf("check system state on %s: %w", fleetHost.name, ctx.Err())
	}
	state := strings.TrimSpace(stdout.String())
	if state == "running" || state == "degraded" {
		return state, nil
	}
	if err != nil {
		return "", fmt.Errorf("check system state on %s: %w", fleetHost.name, err)
	}
	return "", fmt.Errorf("system on %s reports %q, expected running or degraded", fleetHost.name, state)
}
