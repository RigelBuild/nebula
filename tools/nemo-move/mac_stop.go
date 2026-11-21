package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec" //nolint:depguard // SSH is the sole subprocess boundary and receives an absolute executable and argv without a shell.
	"path/filepath"
	"strconv"
	"strings"

	"github.com/RigelBuild/nebula/tools/nemo-move/internal/transfer"
)

const defaultSSHLookup = "ssh"

type commandRunner func(ctx context.Context, name string, args ...string) (string, error)

type launchdJob struct {
	label  string
	domain string
}

var errSystemJobsRemain = errors.New("nemo system launchd jobs remain active or enabled")

const (
	guiDomain    = "gui"
	systemDomain = "system"
)

// launchdLabels is the repeatable --launchd-label flag value.
type launchdLabels []launchdJob

func (l *launchdLabels) String() string {
	parts := make([]string, 0, len(*l))
	for _, job := range *l {
		parts = append(parts, job.label+":"+job.domain)
	}
	return strings.Join(parts, ",")
}

// Set parses one <label>:<gui|system> value.
func (l *launchdLabels) Set(value string) error {
	label, domain, ok := strings.Cut(value, ":")
	if !ok || (domain != guiDomain && domain != systemDomain) {
		return fmt.Errorf("launchd label %q must be <label>:gui or <label>:system", value)
	}
	if !validLaunchdLabel(label) {
		return fmt.Errorf("invalid launchd label %q", label)
	}
	for _, job := range *l {
		if job.label == label {
			return fmt.Errorf("duplicate launchd label %q", label)
		}
	}
	*l = append(*l, launchdJob{label: label, domain: domain})
	return nil
}

func validLaunchdLabel(label string) bool {
	if label == "" || label[0] == '-' || label[0] == '.' {
		return false
	}
	for _, r := range label {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func macProcessNeedles(home string) []string {
	return []string{
		"Nemo.app/Contents/MacOS/Nemo",
		"cotal.js spawn --file",
		"/bin/hermes gateway",
		"--user-data-dir=" + home + "/Library/Application Support/Nemo/Chrome",
		"workspace-mcp --transport stdio",
		"hermes dashboard --host 127.0.0.1 --port 9119",
		"caddy run --adapter caddyfile",
		"cloudflared tunnel --protocol quic",
		"linear-agent-token",
		"nemo-cluster-access-pull",
		"serve --bg --tcp=443 tcp://127.0.0.1:8447",
	}
}

// macStopConfig is the source account and launchd jobs mac stop acts on.
type macStopConfig struct {
	home string
	jobs []launchdJob
}

func sshRunner(ctx context.Context, name string, args ...string) (string, error) {
	if !filepath.IsAbs(name) {
		return "", fmt.Errorf("SSH executable must be an absolute path, got %q", name)
	}
	var stdout, stderr strings.Builder
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // executable path is validated before dispatch
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func mac(ctx context.Context, args []string, runCmd commandRunner) error {
	if len(args) == 0 {
		return errors.New("mac requires stop")
	}
	if args[0] != "stop" {
		return fmt.Errorf("unknown mac step %q", args[0])
	}
	flags := flag.NewFlagSet("mac stop", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	target := flags.String("mac", "", "SSH target user@host")
	sshPath := flags.String("ssh", "", "absolute SSH executable (defaults to ssh from PATH)")
	source := sourceFlags(flags)
	var jobs launchdLabels
	flags.Var(&jobs, "launchd-label", "launchd job to stop as <label>:<gui|system> (required, repeatable)")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("mac stop takes no positional arguments")
	}
	if *target == "" {
		return errors.New("mac stop requires --mac user@host")
	}
	if err := source.check("mac stop", *target); err != nil {
		return err
	}
	if len(jobs) == 0 {
		return errors.New("mac stop requires at least one --launchd-label")
	}
	resolvedSSH, err := resolveSSHPath(*sshPath, exec.LookPath)
	if err != nil {
		return err
	}
	return stopMac(ctx, *target, resolvedSSH, macStopConfig{home: *source.homeRoot, jobs: jobs}, runCmd)
}

func resolveSSHPath(path string, lookPath func(string) (string, error)) (string, error) {
	if path != "" {
		if !filepath.IsAbs(path) {
			return "", errors.New("mac stop requires --ssh to be an absolute executable path")
		}
		return path, nil
	}
	resolved, err := lookPath(defaultSSHLookup)
	if err != nil {
		return "", fmt.Errorf("find ssh executable on PATH: %w", err)
	}
	if !filepath.IsAbs(resolved) {
		resolved, err = filepath.Abs(resolved)
		if err != nil {
			return "", fmt.Errorf("resolve absolute ssh executable path: %w", err)
		}
	}
	return resolved, nil
}

func stopMac(ctx context.Context, target, sshPath string, cfg macStopConfig, runCmd commandRunner) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	user, host, err := transfer.SplitSSHTarget(target)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(sshPath) {
		return errors.New("SSH executable must be an absolute path")
	}
	uidOutput, err := runRemote(ctx, target, sshPath, runCmd, "/usr/bin/id -u")
	if err != nil {
		return fmt.Errorf("read remote user id on %s: %w", host, err)
	}
	uid, err := strconv.Atoi(strings.TrimSpace(uidOutput))
	if err != nil || uid < 1 {
		return fmt.Errorf("invalid user id %q returned by %s", strings.TrimSpace(uidOutput), host)
	}
	gui := fmt.Sprintf("gui/%d", uid)
	if err := requireLaunchdDomain(ctx, target, sshPath, runCmd, gui); err != nil {
		return fmt.Errorf("cannot reach user launchd domain %s on %s; log into the macOS GUI session for %s and retry: %w", gui, host, user, err)
	}
	activity, err := macHasNemoActivity(ctx, target, sshPath, cfg.jobs, runCmd, uid)
	if err != nil {
		return fmt.Errorf("verify system launchd job or user job before stop: %w", err)
	}
	if err := recordLastQuiet(ctx, target, sshPath, runCmd, activity); err != nil {
		return err
	}
	for _, job := range cfg.jobs {
		if job.domain != guiDomain {
			continue
		}
		domain := serviceDomain(job, uid)
		if err := stopJob(ctx, target, sshPath, domain, job, cfg.jobs, runCmd); err != nil {
			return err
		}
	}
	sudoAvailable := true
	if _, err := runRemote(ctx, target, sshPath, runCmd, "sudo -n true"); err != nil {
		sudoAvailable = false
	}
	if !sudoAvailable {
		if err := verifyMacSystemStopped(ctx, target, sshPath, cfg.jobs, uid, runCmd); err != nil {
			if errors.Is(err, errSystemJobsRemain) {
				return manualSystemShutdownError(target, cfg.jobs, err)
			}
			return err
		}
		if err := verifyMacStopped(ctx, target, sshPath, cfg, uid, runCmd); err != nil {
			return err
		}
		return recordLastQuiet(ctx, target, sshPath, runCmd, false)
	}
	for _, job := range cfg.jobs {
		if job.domain != systemDomain {
			continue
		}
		if err := stopJob(ctx, target, sshPath, serviceDomain(job, uid), job, cfg.jobs, runCmd); err != nil {
			return err
		}
	}
	if err := verifyMacStopped(ctx, target, sshPath, cfg, uid, runCmd); err != nil {
		return err
	}
	return recordLastQuiet(ctx, target, sshPath, runCmd, false)
}

func macHasNemoActivity(ctx context.Context, target, sshPath string, jobs []launchdJob, runCmd commandRunner, uid int) (bool, error) {
	for _, job := range jobs {
		loaded, err := launchdJobLoaded(ctx, target, sshPath, serviceDomain(job, uid), runCmd)
		if err != nil {
			return false, err
		}
		if loaded {
			return true, nil
		}
	}
	return false, nil
}

func recordLastQuiet(ctx context.Context, target, sshPath string, runCmd commandRunner, activity bool) error {
	condition := "[ ! -f \"$file\" ]"
	if activity {
		condition = "true"
	}
	script := "set -eu; file=\"$HOME/.local/state/nemo-move/last-quiet\"; /bin/mkdir -p \"${file%/*}\"; if " + condition + "; then /usr/bin/touch \"$file\"; fi"
	if _, err := runRemote(ctx, target, sshPath, runCmd, "/bin/sh -c "+transfer.ShellQuote(script)); err != nil {
		return fmt.Errorf("record last-quiet: %w", err)
	}
	return nil
}

func serviceDomain(job launchdJob, uid int) string {
	if job.domain == guiDomain {
		return fmt.Sprintf("gui/%d/%s", uid, job.label)
	}
	return systemDomain + "/" + job.label
}

func runRemote(ctx context.Context, target, sshPath string, runCmd commandRunner, command string) (string, error) {
	if !filepath.IsAbs(sshPath) {
		return "", errors.New("SSH executable must be an absolute path")
	}
	return runCmd(ctx, sshPath, "-o", "BatchMode=yes", target, command)
}

func requireLaunchdDomain(ctx context.Context, target, sshPath string, runCmd commandRunner, domain string) error {
	script := "if /bin/launchctl print " + transfer.ShellQuote(domain) + " >/dev/null 2>&1; then printf reachable; else printf unavailable; exit 1; fi"
	output, err := runRemote(ctx, target, sshPath, runCmd, "/bin/sh -c "+transfer.ShellQuote(script))
	if err != nil {
		return err
	}
	if strings.TrimSpace(output) != "reachable" {
		return fmt.Errorf("launchd domain %s is unavailable", domain)
	}
	return nil
}

func stopJob(ctx context.Context, target, sshPath, domain string, job launchdJob, jobs []launchdJob, runCmd commandRunner) error {
	loaded, err := launchdJobLoaded(ctx, target, sshPath, domain, runCmd)
	if err != nil {
		return fmt.Errorf("check launchd job %s: %w", domain, err)
	}
	if loaded {
		command := "/bin/launchctl bootout " + domain
		if job.domain == systemDomain {
			command = "sudo -n " + command
		}
		if _, err := runRemote(ctx, target, sshPath, runCmd, command); err != nil {
			if job.domain == systemDomain {
				return manualSystemShutdownError(target, jobs, err)
			}
			return fmt.Errorf("boot out launchd job %s: %w", domain, err)
		}
	}
	command := "/bin/launchctl disable " + domain
	if job.domain == systemDomain {
		command = "sudo -n " + command
	}
	if _, err := runRemote(ctx, target, sshPath, runCmd, command); err != nil {
		if job.domain == systemDomain {
			return manualSystemShutdownError(target, jobs, err)
		}
		return fmt.Errorf("disable launchd job %s: %w", domain, err)
	}
	return nil
}

func launchdJobProbeCommand(domain string) string {
	printCommand := "/bin/launchctl print " + transfer.ShellQuote(domain)
	script := printCommand + " >/dev/null 2>&1; status=$?; if [ \"$status\" -eq 0 ]; then printf loaded; elif [ \"$status\" -eq 113 ]; then printf absent; else printf 'exit:%s' \"$status\"; fi"
	return "/bin/sh -c " + transfer.ShellQuote(script)
}

func launchdJobLoaded(ctx context.Context, target, sshPath, domain string, runCmd commandRunner) (bool, error) {
	output, err := runRemote(ctx, target, sshPath, runCmd, launchdJobProbeCommand(domain))
	if err != nil {
		return false, err
	}
	switch strings.TrimSpace(output) {
	case "loaded":
		return true, nil
	case "absent":
		return false, nil
	default:
		return false, fmt.Errorf("launchctl print %s failed with %s", domain, strings.TrimSpace(output))
	}
}

func manualSystemShutdownError(target string, jobs []launchdJob, err error) error {
	var commands strings.Builder
	for _, job := range jobs {
		if job.domain != systemDomain {
			continue
		}
		domain := serviceDomain(job, 0)
		fmt.Fprintf(&commands, "\n  sudo /bin/launchctl bootout %s\n  sudo /bin/launchctl disable %s", domain, domain)
	}
	return fmt.Errorf("sudo -n could not stop all system launchd jobs: %w\nin an interactive SSH session on %s, run these commands, then rerun nemo-move mac stop:%s", err, target, commands.String())
}

func readDisabledServices(ctx context.Context, target, sshPath, domain string, runCmd commandRunner) (map[string]bool, error) {
	output, err := runRemote(ctx, target, sshPath, runCmd, "/bin/launchctl print-disabled "+domain)
	if err != nil {
		return nil, err
	}
	disabled := make(map[string]bool)
	for line := range strings.SplitSeq(output, "\n") {
		label, state, ok := strings.Cut(line, "=>")
		if !ok || strings.TrimSpace(state) != "disabled" {
			continue
		}
		label = strings.Trim(strings.TrimSpace(label), "\"")
		disabled[label] = true
	}
	return disabled, nil
}

func verifyMacSystemStopped(ctx context.Context, target, sshPath string, jobs []launchdJob, uid int, runCmd commandRunner) error {
	disabled, err := readDisabledServices(ctx, target, sshPath, systemDomain, runCmd)
	if err != nil {
		return fmt.Errorf("verify disabled system jobs: %w", err)
	}
	for _, job := range jobs {
		if job.domain != systemDomain {
			continue
		}
		domain := serviceDomain(job, uid)
		loaded, err := launchdJobLoaded(ctx, target, sshPath, domain, runCmd)
		if err != nil {
			return fmt.Errorf("verify system launchd job %s: %w", domain, err)
		}
		if loaded {
			return fmt.Errorf("%w: launchd job %s remains loaded", errSystemJobsRemain, domain)
		}
		if !disabled[job.label] {
			return fmt.Errorf("%w: launchd job %s is not disabled", errSystemJobsRemain, domain)
		}
	}
	return nil
}

func verifyMacStopped(ctx context.Context, target, sshPath string, cfg macStopConfig, uid int, runCmd commandRunner) error {
	guiDisabled, err := readDisabledServices(ctx, target, sshPath, fmt.Sprintf("gui/%d", uid), runCmd)
	if err != nil {
		return fmt.Errorf("verify disabled user jobs in gui/%d: %w", uid, err)
	}
	systemDisabled, err := readDisabledServices(ctx, target, sshPath, systemDomain, runCmd)
	if err != nil {
		return fmt.Errorf("verify disabled system jobs: %w", err)
	}
	for _, job := range cfg.jobs {
		domain := serviceDomain(job, uid)
		loaded, err := launchdJobLoaded(ctx, target, sshPath, domain, runCmd)
		if err != nil {
			if job.domain == systemDomain {
				return fmt.Errorf("verify system launchd job %s: %w", domain, err)
			}
			return fmt.Errorf("verify launchd job %s: %w", domain, err)
		}
		if loaded {
			return fmt.Errorf("nemo stop incomplete: launchd job %s remains loaded", domain)
		}
		disabled := guiDisabled
		if job.domain == systemDomain {
			disabled = systemDisabled
		}
		if !disabled[job.label] {
			return fmt.Errorf("nemo stop incomplete: launchd job %s is not disabled", domain)
		}
	}
	processes, err := runRemote(ctx, target, sshPath, runCmd, "/bin/ps -axo pid=,command=")
	if err != nil {
		return fmt.Errorf("verify Nemo processes on %s: %w", target, err)
	}
	for _, needle := range macProcessNeedles(cfg.home) {
		if strings.Contains(processes, needle) {
			return fmt.Errorf("nemo stop incomplete: a process remains matching %q", needle)
		}
	}
	return nil
}
