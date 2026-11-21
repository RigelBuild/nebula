package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/client"
	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/config"
	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/fence"
	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/inventory"
	"github.com/spf13/cobra"
)

type passOptions struct {
	host, configPath, stateDir, repo string
	escrowOut, escrowRecipient       string
	restic                           string
}

func newPassCmd() *cobra.Command {
	var opts passOptions
	command := &cobra.Command{
		Use:   "pass",
		Short: "Inventory this host and archive changed transcripts in one restic pass",
		Long: `pass inventories the host, opens the scope state in --state-dir, recovers any
unfinished pass, records a scan and runs one restic backup. Run it from a timer.

The repository password is generated once in --state-dir and escrowed,
age-encrypted, to --escrow-out before the repository is initialized. RESTIC_*
variables in the environment are ignored. For an R2 (s3:) repository, set
AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY; they are passed through to restic.

Exit 0 means the pass was acked. Any other result exits non-zero with one line
on stderr; the next run recovers.`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runPass(command.Context(), opts)
		},
	}
	command.Flags().StringVar(&opts.host, "host", "", "host name from the archive config (also the archive scope)")
	command.Flags().StringVar(&opts.configPath, "config", "", "archive CUE config path (required)")
	command.Flags().StringVar(&opts.stateDir, "state-dir", "", "private scope state directory")
	command.Flags().StringVar(&opts.repo, "repo", "", "restic repository")
	command.Flags().StringVar(&opts.escrowOut, "escrow-out", "", "age-encrypted password escrow file (written once)")
	command.Flags().StringVar(&opts.escrowRecipient, "escrow-recipient", "", "age recipient for the escrow (required)")
	command.Flags().StringVar(&opts.restic, "restic", "restic", "restic binary (default resolves restic on PATH)")
	return command
}

func runPass(ctx context.Context, opts passOptions) (err error) {
	if opts.host == "" || opts.configPath == "" || opts.stateDir == "" || opts.repo == "" || opts.escrowOut == "" || opts.escrowRecipient == "" {
		return errors.New("pass requires host, config, state-dir, repo, escrow-out and escrow-recipient")
	}
	escrow, err := newFileEscrower(opts.escrowOut, opts.escrowRecipient)
	if err != nil {
		return err
	}
	walk, empty, err := hostWalk(opts.host, opts.configPath)
	if err != nil || empty {
		return err
	}
	scratch := filepath.Join(opts.stateDir, "scratch")
	for _, dir := range []string{opts.stateDir, scratch} {
		if err := refuseUnderSource(walk, dir); err != nil {
			return err
		}
	}
	// One pass per state dir: a manual run beside the timer must not interleave journal writes.
	release, err := fence.Lock(ctx, opts.stateDir, time.Now())
	if err != nil {
		return fmt.Errorf("lock state directory: %w", err)
	}
	defer func() { err = errors.Join(err, release()) }()
	c, err := client.Open(opts.stateDir, client.RepoRef{Scope: opts.host, Repository: opts.repo}, escrow,
		client.WithScratch(scratch), client.WithRestic(opts.restic))
	if err != nil {
		return err
	}
	// Open replays and republishes scan-current.jsonl, so it is the last committed scan.
	var previous *inventory.Inventory
	if prior, err := readInventory(filepath.Join(opts.stateDir, "scan-current.jsonl")); err == nil {
		previous = prior
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read previous inventory: %w", err)
	}
	current, delta, err := inventory.Walk(walk, previous)
	if err != nil {
		return err
	}
	if err := c.Recover(ctx); err != nil {
		return err
	}
	if err := c.Scan(ctx, current, delta); err != nil {
		return err
	}
	// Every live non-final-only file goes in, so a file left pending by a failed pass is retried; restic skips unchanged ones.
	files := make([]string, 0, len(current.Lines))
	for _, line := range current.Lines {
		if line.Type == "file" && line.Class != "final-only" {
			files = append(files, line.Path)
		}
	}
	outcome, err := c.Pass(ctx, files, nil)
	if err != nil {
		return err
	}
	if outcome.Kind != client.Acked {
		return fmt.Errorf("pass %s not acked (kind %d, restic exit %d)", outcome.PassID, outcome.Kind, outcome.ExitCode)
	}
	return nil
}

// refuseUnderSource rejects dir when it or any ancestor matches a source root, so a pass never archives its own state.
func refuseUnderSource(walk config.Walk, dir string) error {
	real, err := resolveExisting(dir)
	if err != nil {
		return err
	}
	for _, source := range walk.Sources {
		pattern, err := inventory.SourcePath(walk, source.Path)
		if err != nil {
			return err
		}
		patterns := []string{pattern}
		if resolved, err := resolveExisting(pattern); err == nil && resolved != pattern {
			patterns = append(patterns, resolved)
		}
		for path := real; ; path = filepath.Dir(path) {
			for _, pattern := range patterns {
				if matched, err := filepath.Match(pattern, path); err != nil {
					return fmt.Errorf("source %q pattern: %w", source.Name, err)
				} else if matched {
					return fmt.Errorf("%s is under source %q; move it outside the walked roots", real, source.Name)
				}
			}
			if parent := filepath.Dir(path); parent == path {
				break
			}
		}
	}
	return nil
}

// resolveExisting makes path absolute and resolves symlinks in its longest existing prefix.
func resolveExisting(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", path, err)
	}
	rest := ""
	for prefix := abs; ; prefix = filepath.Dir(prefix) {
		resolved, err := filepath.EvalSymlinks(prefix)
		if err == nil {
			return filepath.Join(resolved, rest), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("resolve %s: %w", prefix, err)
		}
		if filepath.Dir(prefix) == prefix {
			return abs, nil
		}
		rest = filepath.Join(filepath.Base(prefix), rest)
	}
}
