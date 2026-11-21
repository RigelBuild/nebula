package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/coverage"
	"github.com/spf13/cobra"
)

type coverageOptions struct {
	final, baseline, digests, vanished  string
	scope, snapshot, repo, passwordFile string
	restic, stateDir, cacheDir          string
	deltas, journals                    []string
	chained                             bool
}

func newCoverageCmd() *cobra.Command {
	var opts coverageOptions
	command := &cobra.Command{
		Use:   "coverage",
		Short: "Check transcript archive coverage",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runCoverage(command.OutOrStdout(), opts)
		},
	}
	command.Flags().StringVar(&opts.final, "final", "", "final inventory JSONL path")
	command.Flags().StringVar(&opts.baseline, "baseline", "", "baseline inventory JSONL path")
	command.Flags().StringArrayVar(&opts.deltas, "deltas", nil, "scan delta JSONL path (repeat for pass order)")
	command.Flags().BoolVar(&opts.chained, "chained", false, "check a chained seat lifecycle")
	command.Flags().StringVar(&opts.scope, "scope", "", "archive scope")
	command.Flags().StringVar(&opts.snapshot, "snapshot", "", "final snapshot ID")
	command.Flags().StringVar(&opts.repo, "repo", "", "restic repository")
	command.Flags().StringVar(&opts.passwordFile, "password-file", "", "restic password file")
	command.Flags().StringArrayVar(&opts.journals, "journal", nil, "pass journal JSONL path (repeat for each journal)")
	command.Flags().StringVar(&opts.digests, "digests", "", "snapshot path digest JSON file")
	command.Flags().StringVar(&opts.vanished, "vanished", "", "journaled vanished paths file")
	command.Flags().StringVar(&opts.restic, "restic", "restic", "restic binary (default resolves restic on PATH)")
	command.Flags().StringVar(&opts.stateDir, "state-dir", "", "pass state directory: check every live file against the latest acked snapshot (dumps only unverified files)")
	return command
}

func runCoverage(out io.Writer, opts coverageOptions) error {
	if opts.stateDir != "" {
		if opts.scope == "" || opts.repo == "" {
			return errors.New("coverage --state-dir requires scope and repo")
		}
		return runStateCoverage(out, opts)
	}
	if opts.final == "" || opts.baseline == "" || opts.snapshot == "" || opts.scope == "" || opts.repo == "" || opts.passwordFile == "" || len(opts.journals) == 0 || opts.digests == "" || opts.vanished == "" || len(opts.deltas) == 0 {
		return errors.New("coverage requires --state-dir, or final, baseline, deltas, scope, snapshot, repo, password-file, journal, digests and vanished")
	}
	final, err := readInventory(opts.final)
	if err != nil {
		return fmt.Errorf("read final inventory: %w", err)
	}
	baseline, err := readInventory(opts.baseline)
	if err != nil {
		return fmt.Errorf("read baseline inventory: %w", err)
	}
	digests, err := readDigests(opts.digests)
	if err != nil {
		return err
	}
	vanished, err := readVanished(opts.vanished)
	if err != nil {
		return fmt.Errorf("read vanished paths: %w", err)
	}
	listing, err := resticListing(opts, opts.snapshot, digests)
	if err != nil {
		return err
	}
	if listing.StateFiles, err = stateFiles(opts); err != nil {
		return err
	}
	history, err := resticHistory(opts)
	if err != nil {
		return err
	}
	readers := make([]*os.File, 0, len(opts.deltas))
	for _, path := range opts.deltas {
		file, err := os.Open(path) //nolint:gosec // Operator-supplied delta path.
		if err != nil {
			return errors.Join(fmt.Errorf("open scan delta: %w", err), closeFiles(readers))
		}
		readers = append(readers, file)
	}
	inputs := make([]coverage.Delta, len(readers))
	for i, reader := range readers {
		name := filepath.Base(opts.deltas[i])
		passID := strings.TrimSuffix(strings.TrimPrefix(name, "delta-"), ".jsonl")
		if name != "delta-"+passID+".jsonl" || passID == "" {
			return errors.Join(errors.New("invalid scan delta filename"), closeFiles(readers))
		}
		inputs[i] = coverage.Delta{PassID: passID, R: reader}
	}
	report := coverage.Check(*final, *baseline, inputs, opts.chained, listing, digests, history, vanished)
	if err := closeFiles(readers); err != nil {
		return fmt.Errorf("close scan deltas: %w", err)
	}
	return writeCoverageReport(out, report)
}

// stateFiles names the archive's own files that a pass backs up; coverage exempts exactly these.
func stateFiles(opts coverageOptions) (map[string]struct{}, error) {
	files := make(map[string]struct{}, len(opts.deltas)+len(opts.journals))
	for _, path := range append(append([]string(nil), opts.deltas...), opts.journals...) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve state file: %w", err)
		}
		files[filepath.ToSlash(abs)] = struct{}{}
	}
	return files, nil
}

func writeCoverageReport(out io.Writer, report coverage.Report) error {
	output, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("encode coverage report: %w", err)
	}
	if _, err := out.Write(append(output, '\n')); err != nil {
		return fmt.Errorf("write coverage report: %w", err)
	}
	if !report.Pass {
		return errors.New("transcript archive coverage failed")
	}
	return nil
}

func readDigests(path string) (map[string]coverage.FileDigest, error) {
	file, err := os.Open(path) //nolint:gosec // Path is an operator-supplied CLI flag.
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = file.Close()
	}()
	var digests map[string]coverage.FileDigest
	if err := json.NewDecoder(file).Decode(&digests); err != nil {
		return nil, err
	}
	return digests, nil
}

func readVanished(path string) ([]string, error) {
	file, err := os.Open(path) //nolint:gosec // Path is an operator-supplied CLI flag.
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = file.Close()
	}()
	paths := []string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if scanner.Text() != "" {
			paths = append(paths, scanner.Text())
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return paths, nil
}

func closeFiles(files []*os.File) error {
	var errs []error
	for _, file := range files {
		if err := file.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
