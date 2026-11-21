package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

func main() {
	os.Exit(runMain())
}

func runMain() int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return run(ctx, os.Args[1:], os.Stdout, os.Stderr)
}

type exitError struct {
	code int
	err  error
}

func (err exitError) Error() string { return err.err.Error() }
func (err exitError) Unwrap() error { return err.err }

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	deps := newDependencies(stdout, stderr)
	command := newRootCommand(ctx, args, deps)
	command.SetOut(stdout)
	command.SetErr(stderr)
	if err := command.Execute(); err != nil {
		if _, writeErr := fmt.Fprintln(stderr, err); writeErr != nil {
			return 2
		}
		if coded, ok := errors.AsType[exitError](err); ok {
			return coded.code
		}
		return 2
	}
	return 0
}

func newRootCommand(ctx context.Context, argv []string, deps dependencies) *cobra.Command {
	var hostsFile string
	root := &cobra.Command{
		Use:           "fleet-switch",
		Short:         "Switch or reboot Nix fleet hosts",
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.NoArgs,
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			var err error
			fleetHosts, err = loadHosts(hostsFile)
			if err != nil {
				return exitError{code: 2, err: err}
			}
			return nil
		},
		RunE: func(command *cobra.Command, _ []string) error {
			return command.Help()
		},
	}
	root.SetArgs(argv)
	root.PersistentFlags().StringVar(&hostsFile, "hosts", "", "CUE inventory file of fleet hosts (required)")
	root.PersistentFlags().StringVar(&repoURL, "repo-url", "", "Trunk git repository URL to pin and deploy from (required for trunk deploys)")
	root.AddCommand(newListCommand(), newSwitchCommand(ctx, deps), newBootCommand(ctx, deps))
	return root
}

func newListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List known fleet hosts",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			for _, fleetHost := range fleetHosts {
				if _, err := fmt.Fprintf(command.OutOrStdout(), "%s\t%s\n", fleetHost.name, fleetHost.platform); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newSwitchCommand(ctx context.Context, deps dependencies) *cobra.Command {
	var all bool
	var except []string
	var flake string
	var allowLocal bool
	var dryRun bool
	command := &cobra.Command{
		Use:   "switch [HOST...]",
		Short: "Switch one or more fleet hosts",
		Long:  "Switch selected fleet hosts in order. Specify hosts or use --all.",
		RunE: func(cmd *cobra.Command, hosts []string) error {
			selected, skipped, err := selectHosts(hosts, all, except)
			if err != nil {
				return exitError{code: 2, err: err}
			}
			deps.skippedHosts = skipped
			src, err := resolveSource(ctx, deps, flake, cmd.Flags().Changed("flake"))
			if err != nil {
				return exitError{code: 2, err: err}
			}
			if err := checkSource(selected, src, allowLocal); err != nil {
				return exitError{code: 2, err: err}
			}
			if err := printSource(deps.stdout, src); err != nil {
				return exitError{code: 1, err: err}
			}
			if err := runSwitch(ctx, deps, src, selected, dryRun); err != nil {
				return exitError{code: 1, err: err}
			}
			return nil
		},
	}
	command.Flags().BoolVar(&all, "all", false, "Select every known host")
	command.Flags().StringArrayVar(&except, "except", nil, "Skip HOST (repeatable)")
	command.Flags().StringVar(&flake, "flake", "", "Deploy this repository root instead of trunk (localCheckout hosts only; see --allow-local)")
	command.Flags().BoolVar(&allowLocal, "allow-local", false, "Allow --flake PATH to deploy to hosts other than localCheckout hosts")
	command.Flags().BoolVar(&dryRun, "dry-run", false, "Print commands without running them")
	return command
}

func newBootCommand(ctx context.Context, deps dependencies) *cobra.Command {
	var all bool
	var except []string
	var flake string
	var allowLocal bool
	var noReboot bool
	var dryRun bool
	var timeout time.Duration
	command := &cobra.Command{
		Use:   "boot [HOST...]",
		Short: "Install boot configurations and reboot hosts sequentially",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, hosts []string) error {
			selected, skipped, err := selectHosts(hosts, all, except)
			if err != nil {
				return exitError{code: 2, err: err}
			}
			deps.skippedHosts = skipped
			src, err := resolveSource(ctx, deps, flake, cmd.Flags().Changed("flake"))
			if err != nil {
				return exitError{code: 2, err: err}
			}
			if err := checkSource(selected, src, allowLocal); err != nil {
				return exitError{code: 2, err: err}
			}
			if err := printSource(deps.stdout, src); err != nil {
				return exitError{code: 1, err: err}
			}
			if err := runBoot(ctx, deps, src, selected, noReboot, dryRun, timeout); err != nil {
				return exitError{code: 1, err: err}
			}
			return nil
		},
	}
	command.Flags().BoolVar(&all, "all", false, "Select every known host")
	command.Flags().StringArrayVar(&except, "except", nil, "Skip HOST (repeatable)")
	command.Flags().StringVar(&flake, "flake", "", "Deploy this repository root instead of trunk (localCheckout hosts only; see --allow-local)")
	command.Flags().BoolVar(&allowLocal, "allow-local", false, "Allow --flake PATH to deploy to hosts other than localCheckout hosts")
	command.Flags().BoolVar(&noReboot, "no-reboot", false, "Install boot configuration without rebooting")
	command.Flags().DurationVar(&timeout, "timeout", 15*time.Minute, "Limit each host's entire boot operation")
	command.Flags().BoolVar(&dryRun, "dry-run", false, "Print commands without running them")
	return command
}

func runSwitch(ctx context.Context, deps dependencies, src deploySource, selected []host, dryRun bool) error {
	if err := printSkippedHosts(deps); err != nil {
		return err
	}
	failed := make([]string, 0)
	skipped := 0
	for _, fleetHost := range selected {
		hostSkipped, hostFailed, err := switchHost(ctx, deps, src, fleetHost, dryRun)
		if err != nil {
			return err
		}
		if hostSkipped {
			skipped++
		}
		if hostFailed {
			failed = append(failed, fleetHost.name)
		}
	}
	return printSwitchSummary(deps.stdout, selected, dryRun, failed, skipped)
}

func switchHost(ctx context.Context, deps dependencies, src deploySource, fleetHost host, dryRun bool) (bool, bool, error) {
	if err := ctx.Err(); err != nil {
		return false, false, fmt.Errorf("switch interrupted before %s: %w", fleetHost.name, err)
	}
	if err := printHostHeader(deps.stdout, fleetHost); err != nil {
		return false, false, err
	}
	skip, err := gamingSkip(ctx, deps, fleetHost, dryRun)
	if err != nil {
		if ctx.Err() != nil {
			return false, false, fmt.Errorf("switch interrupted during gaming probe on %s; no changes made: %w", fleetHost.name, ctx.Err())
		}
		if _, printErr := fmt.Fprintln(deps.stderr, err); printErr != nil {
			return false, false, printErr
		}
		return false, true, nil
	}
	if skip {
		return true, false, nil
	}
	if fleetHost.platform == platformDarwin {
		failed, err := switchDarwin(ctx, deps, src, fleetHost, dryRun)
		return false, failed, err
	}
	if fleetHost.platform == platformWindows {
		failed, err := switchWindows(ctx, deps, src, fleetHost, dryRun)
		return false, failed, err
	}
	cmd := nixosRebuildCommand("switch", src, fleetHost)
	if dryRun {
		_, err := fmt.Fprintln(deps.stdout, formatCommand(cmd))
		return false, false, err
	}
	if err := deps.runner(ctx, cmd.name, cmd.args, deps.stdout, deps.stderr); err != nil {
		if ctx.Err() != nil {
			return false, false, fmt.Errorf("switch interrupted on %s; remote state unknown: %w", fleetHost.name, ctx.Err())
		}
		return false, true, nil
	}
	return false, false, nil
}

// switchWindows updates the Windows checkout, then runs windows-setup.ps1; a failed step fails the host.
func switchWindows(ctx context.Context, deps dependencies, src deploySource, fleetHost host, dryRun bool) (bool, error) {
	cmds := windowsSwitchCommands(src, fleetHost)
	if dryRun {
		for i, script := range windowsSwitchScripts(src, fleetHost) {
			if _, err := fmt.Fprintln(deps.stdout, formatCommand(cmds[i])); err != nil {
				return false, err
			}
			if err := printPowerShellScript(deps.stdout, script); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	for _, cmd := range cmds {
		if err := deps.runner(ctx, cmd.name, cmd.args, deps.stdout, deps.stderr); err != nil {
			if ctx.Err() != nil {
				return false, fmt.Errorf("switch interrupted on %s; remote state unknown: %w", fleetHost.name, ctx.Err())
			}
			_, printErr := fmt.Fprintf(deps.stderr, "%s: %v\n", fleetHost.name, err)
			return true, printErr
		}
	}
	return false, nil
}

// printPowerShellScript prints script as comment lines under the command that runs it.
func printPowerShellScript(out io.Writer, script string) error {
	if _, err := fmt.Fprintln(out, "# pwsh script:"); err != nil {
		return err
	}
	for _, line := range strings.Split(script, "\n") {
		if _, err := fmt.Fprintf(out, "#   %s\n", line); err != nil {
			return err
		}
	}
	return nil
}

func printSwitchSummary(out io.Writer, selected []host, dryRun bool, failed []string, skipped int) error {
	if _, err := fmt.Fprintln(out); err != nil {
		return err
	}
	if len(failed) > 0 {
		if _, err := fmt.Fprintf(out, "✗ Failed: %s\n", strings.Join(failed, " ")); err != nil {
			return err
		}
		return fmt.Errorf("switch failed for hosts: %s", strings.Join(failed, ", "))
	}
	if dryRun {
		_, err := fmt.Fprintln(out, "✓ Dry run complete.")
		return err
	}
	if skipped > 0 {
		_, err := fmt.Fprintf(out, "✓ switched %d, skipped %d (gaming)\n", len(selected)-skipped, skipped)
		return err
	}
	_, err := fmt.Fprintln(out, "✓ All hosts switched successfully.")
	return err
}

func runBoot(ctx context.Context, deps dependencies, src deploySource, selected []host, noReboot, dryRun bool, timeout time.Duration) error {
	if timeout <= 0 {
		return errors.New("--timeout must be greater than zero")
	}
	for _, fleetHost := range selected {
		if !fleetHost.rebootable {
			return fmt.Errorf("boot does not support non-rebootable host %q", fleetHost.name)
		}
	}
	if err := printSkippedHosts(deps); err != nil {
		return err
	}
	for _, fleetHost := range selected {
		if err := runBootHost(ctx, deps, src, fleetHost, noReboot, dryRun, timeout); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(deps.stdout); err != nil {
		return err
	}
	if dryRun {
		_, err := fmt.Fprintln(deps.stdout, "✓ Dry run complete.")
		return err
	}
	_, err := fmt.Fprintln(deps.stdout, "✓ Boot completed for all selected hosts.")
	return err
}

func runBootHost(ctx context.Context, deps dependencies, src deploySource, fleetHost host, noReboot, dryRun bool, timeout time.Duration) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("boot interrupted before %s: %w", fleetHost.name, err)
	}
	if err := printHostHeader(deps.stdout, fleetHost); err != nil {
		return err
	}
	skip, err := gamingSkip(ctx, deps, fleetHost, dryRun)
	if err != nil {
		return err
	}
	if skip {
		return nil
	}
	build := nixosRebuildCommand("boot", src, fleetHost)
	if dryRun {
		return printBootDryRun(deps.stdout, build, fleetHost, noReboot, timeout)
	}
	if err := bootHost(ctx, deps, fleetHost, build, noReboot, timeout); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("boot interrupted on %s; remote state unknown: %w", fleetHost.name, ctx.Err())
		}
		return err
	}
	return nil
}

func printSkippedHosts(deps dependencies) error {
	for _, fleetHost := range deps.skippedHosts {
		if _, err := fmt.Fprintf(deps.stdout, "==> %s (skipped)\n", fleetHost.name); err != nil {
			return err
		}
	}
	return nil
}

func printSource(out io.Writer, src deploySource) error {
	if src.isLocal() {
		_, err := fmt.Fprintf(out, "Flake: %s\nwarning: local checkout; the host will drift from main until it is switched from trunk\n", src.root)
		return err
	}
	_, err := fmt.Fprintf(out, "Deploying %s main at %s\n", repoURL, src.rev)
	return err
}

func printHostHeader(out io.Writer, fleetHost host) error {
	if _, err := fmt.Fprintln(out); err != nil {
		return err
	}
	_, err := fmt.Fprintf(out, "==> %s (%s, %s)\n", fleetHost.name, fleetHost.target, fleetHost.platform)
	return err
}

func printBootDryRun(out io.Writer, build command, fleetHost host, noReboot bool, timeout time.Duration) error {
	if _, err := fmt.Fprintln(out, formatCommand(build)); err != nil {
		return err
	}
	if noReboot {
		return nil
	}
	if _, err := fmt.Fprintln(out, formatCommand(bootIDCommand(fleetHost))); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(out, formatCommand(rebootCommand(fleetHost))); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "# poll for a changed boot ID (timeout %s): %s\n", timeout, formatCommand(bootIDCommand(fleetHost))); err != nil {
		return err
	}
	_, err := fmt.Fprintln(out, formatCommand(healthCommand(fleetHost)))
	return err
}
