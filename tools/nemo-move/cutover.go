package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec" //nolint:depguard // Resolves the SSH executable.

	"github.com/RigelBuild/nebula/tools/nemo-move/internal/transfer"
)

type cutoverOperations interface {
	open(context.Context) error
	close(context.Context) error
	record(context.Context) (string, string, error)
	readPhase(context.Context) (string, error)
	writePhase(context.Context, string) error
	writeRecord(context.Context, string, string) error
	checkHome(context.Context, string) error
	stop(context.Context, string) error
	export(context.Context, string) error
	extract(context.Context, func() error) error
	verify(context.Context) error
	cleanup(context.Context, string) error
}

type cutoverDeps struct {
	ops   cutoverOperations
	newID func() (string, error)
	fault func(string) error
}

func cutover(ctx context.Context, args []string, deps cutoverDeps, stdout, stderr io.Writer) (result error) {
	flags := flag.NewFlagSet("cutover", flag.ContinueOnError)
	flags.SetOutput(stderr)
	kubeconfig := flags.String("kubeconfig", "", "admin kubeconfig")
	mac := flags.String("mac", "", "source host")
	macBinary := flags.String("mac-binary", "", "macOS nemo-move binary")
	resume := flags.String("resume", "", "committing run ID")
	source := sourceFlags(flags)
	var jobs launchdLabels
	flags.Var(&jobs, "launchd-label", "launchd job to stop as <label>:<gui|system> (required, repeatable)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *kubeconfig == "" || *mac == "" || *macBinary == "" || len(jobs) == 0 {
		return errors.New("cutover requires --kubeconfig, --mac, --mac-binary, --home-root, --ssh-user and --launchd-label")
	}
	opts, err := source.check("cutover", *mac)
	if err != nil {
		return err
	}
	if deps.ops == nil {
		deps.ops = &productionCutover{ctl: controlPath{run: kubectlRun(*kubeconfig)}, macBinary: *macBinary, source: opts, jobs: jobs, stderr: stderr}
	}
	if deps.newID == nil {
		deps.newID = newCutoverID
	}
	ops := deps.ops
	if p, ok := ops.(*productionCutover); ok {
		if err := p.prepareArchiveSpool(); err != nil {
			return fmt.Errorf("prepare operator archive spool: %w", err)
		}
		defer func() { result = errors.Join(result, p.removeArchive()) }()
	}
	if err := ops.open(ctx); err != nil {
		return fmt.Errorf("open maintenance path: %w", err)
	}
	id, state, err := ops.record(ctx)
	if err != nil {
		return errors.Join(fmt.Errorf("read cutover record: %w", err), ops.close(ctx))
	}
	if state == "committing" {
		if *resume == "" || *resume != id {
			return errors.Join(errors.New("committing cutover requires matching --resume"), ops.close(ctx))
		}
		value, err := ops.readPhase(ctx)
		if err != nil {
			return fmt.Errorf("read phase: %w", err)
		}
		if value != "owner" {
			if err := cutoverCommitOwner(ctx, deps); err != nil {
				return err
			}
		}
		if err := ops.writeRecord(ctx, id, "committed"); err != nil {
			return fmt.Errorf("write committed record: %w", err)
		}
		return ops.close(ctx)
	}
	if *resume != "" {
		return errors.Join(errors.New("--resume requires a committing record"), ops.close(ctx))
	}
	value, err := ops.readPhase(ctx)
	if err != nil {
		return fmt.Errorf("read phase: %w", err)
	}
	if value == "owner" {
		return errors.Join(errors.New("phase already owner"), ops.close(ctx))
	}
	if err := ops.checkHome(ctx, *mac); err != nil {
		return errors.Join(fmt.Errorf("check source home: %w", err), ops.close(ctx))
	}
	if err := ops.stop(ctx, *mac); err != nil {
		return fmt.Errorf("stop mac: %w", err)
	}
	if err := ops.export(ctx, *mac); err != nil {
		return fmt.Errorf("export mac: %w", err)
	}
	id, err = deps.newID()
	if err != nil {
		return fmt.Errorf("generate cutover run ID: %w", err)
	}
	if err := ops.writeRecord(ctx, id, "started"); err != nil {
		return fmt.Errorf("write started record: %w", err)
	}
	if err := ops.writePhase(ctx, "blocked"); err != nil {
		return fmt.Errorf("write blocked phase: %w", err)
	}
	if err := cutoverFault(deps, "before-extract"); err != nil {
		return err
	}
	recordID, recordState, err := ops.record(ctx)
	if err != nil {
		return fmt.Errorf("read cutover record before extract: %w", err)
	}
	phase, err := ops.readPhase(ctx)
	if err != nil {
		return fmt.Errorf("read phase before extract: %w", err)
	}
	if recordID != id || recordState != "started" || phase != "blocked" {
		return errors.New("extract requires matching started record and blocked phase")
	}
	if err := ops.extract(ctx, func() error { return cutoverFault(deps, "mid-extract") }); err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	if err := ops.verify(ctx); err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	if err := cutoverFault(deps, "after-verify-before-commit"); err != nil {
		return err
	}
	if err := ops.writeRecord(ctx, id, "committing"); err != nil {
		return fmt.Errorf("write committing record: %w", err)
	}
	if err := cutoverCommitOwner(ctx, deps); err != nil {
		return err
	}
	if err := ops.writeRecord(ctx, id, "committed"); err != nil {
		return fmt.Errorf("write committed record: %w", err)
	}
	if err := ops.cleanup(ctx, *mac); err != nil {
		return fmt.Errorf("clean staging: %w", err)
	}
	return ops.close(ctx)
}

func cutoverFault(deps cutoverDeps, point string) error {
	if deps.fault != nil {
		return deps.fault(point)
	}
	return nil
}

func cutoverCommitOwner(ctx context.Context, deps cutoverDeps) error {
	writeErr := deps.ops.writePhase(ctx, "owner")
	ackErr := cutoverFault(deps, "after-phase-write-before-ack")
	value, readErr := deps.ops.readPhase(ctx)
	if readErr != nil {
		return fmt.Errorf("read owner phase: %w", errors.Join(writeErr, ackErr, readErr))
	}
	if value != "owner" {
		return fmt.Errorf("phase read back as %q: %w", value, errors.Join(writeErr, ackErr, errors.New("owner not confirmed")))
	}
	return nil
}

type productionCutover struct {
	macBinary  string
	source     transfer.Options
	jobs       []launchdJob
	archiveDir string
	ctl        controlPath
	stderr     io.Writer
	archive    *os.File
	manifest   []byte
}

func newCutoverID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

func (p *productionCutover) open(ctx context.Context) error {
	return p.ctl.open(ctx)
}

func (p *productionCutover) close(ctx context.Context) error {
	return p.ctl.close(ctx)
}

func (p *productionCutover) maint(ctx context.Context, args ...string) ([]byte, error) {
	return p.ctl.maint(ctx, nil, args...)
}

func (p *productionCutover) record(ctx context.Context) (string, string, error) {
	data, err := p.maint(ctx, "cutover-read")
	if err != nil || len(data) == 0 {
		return "", "", err
	}
	var record struct {
		RunID string `json:"runID"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return "", "", err
	}
	return record.RunID, record.State, nil
}
func (p *productionCutover) readPhase(ctx context.Context) (string, error) {
	data, err := p.maint(ctx, "phase-read")
	return string(data), err
}
func (p *productionCutover) writePhase(ctx context.Context, value string) error {
	_, err := p.maint(ctx, "phase-write", value)
	return err
}
func (p *productionCutover) writeRecord(ctx context.Context, id, state string) error {
	_, err := p.maint(ctx, "cutover-write", id, state)
	return err
}
func (p *productionCutover) checkHome(ctx context.Context, target string) error {
	ssh, err := resolveSSHPath("", exec.LookPath)
	if err != nil {
		return err
	}
	return transfer.CheckSourceHome(ctx, p.source, target, ssh, transfer.SSHRemote())
}
func (p *productionCutover) stop(ctx context.Context, target string) error {
	ssh, err := resolveSSHPath("", exec.LookPath)
	if err != nil {
		return err
	}
	return stopMac(ctx, target, ssh, macStopConfig{home: p.source.SourceHome, jobs: p.jobs, processes: p.source.Layout.Processes}, sshRunner)
}
func (p *productionCutover) prepareArchiveSpool() error {
	directory, err := os.MkdirTemp("", "nemo-move-cutover-*")
	if err != nil {
		return fmt.Errorf("create per-run operator spool: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return errors.Join(fmt.Errorf("secure per-run operator spool: %w", err), os.RemoveAll(directory))
	}
	p.archiveDir = directory
	return nil
}

func (p *productionCutover) export(ctx context.Context, target string) error {
	archive, err := os.CreateTemp(p.archiveDir, "nemo-cutover-*.tar")
	if err != nil {
		return err
	}
	p.archive = archive
	ssh, err := resolveSSHPath("", exec.LookPath)
	if err != nil {
		return err
	}
	return transfer.ExportToSpool(ctx, p.source, target, ssh, p.macBinary, p.archiveDir, transfer.SSHRemote(), archive, p.stderr)
}

func (p *productionCutover) removeArchive() error {
	var cleanupErr error
	if p.archive != nil {
		cleanupErr = errors.Join(p.archive.Close(), os.Remove(p.archive.Name()))
	}
	if p.archiveDir != "" {
		cleanupErr = errors.Join(cleanupErr, os.RemoveAll(p.archiveDir))
	}
	return cleanupErr
}

func (p *productionCutover) extract(ctx context.Context, fault func() error) error {
	if err := fault(); err != nil {
		return err
	}
	if _, err := p.archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	layoutJSON, err := p.source.Layout.JSON()
	if err != nil {
		return err
	}
	manifest, err := p.ctl.execIn(ctx, p.archive, "nemo-move", "maint-side", "extract", p.source.SourceHome, layoutJSON)
	if err != nil {
		return err
	}
	p.manifest = manifest
	return nil
}
func (p *productionCutover) verify(ctx context.Context) error {
	layoutJSON, err := p.source.Layout.JSON()
	if err != nil {
		return err
	}
	_, err = p.ctl.execIn(ctx, bytes.NewReader(p.manifest), "nemo-move", "verify-stream", layoutJSON)
	return err
}
func (p *productionCutover) cleanup(ctx context.Context, target string) error {
	ssh, err := resolveSSHPath("", exec.LookPath)
	if err != nil {
		return err
	}
	_, err = sshRunner(ctx, ssh, "-o", "BatchMode=yes", target, "/bin/rm -rf "+transfer.ShellQuote(p.source.SourceHome+"/.local/state/nemo-move-staging"))
	return err
}
