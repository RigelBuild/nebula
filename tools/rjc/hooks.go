package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
)

const hkConfigName = "hk.pkl"
const (
	hkKind        = "hk"
	lefthookKind  = "lefthook"
	preCommitKind = "pre-commit"
	prekKind      = "prek"
)

type hookConfig struct{ path, kind string }

var hookConfigs = []hookConfig{
	{path: hkConfigName, kind: hkKind},
	{path: "lefthook.yml", kind: lefthookKind}, {path: "lefthook.yaml", kind: lefthookKind},
	{path: "lefthook.json", kind: lefthookKind}, {path: "lefthook.jsonc", kind: lefthookKind}, {path: "lefthook.toml", kind: lefthookKind},
	{path: ".lefthook.yml", kind: lefthookKind}, {path: ".lefthook.yaml", kind: lefthookKind},
	{path: ".lefthook.json", kind: lefthookKind}, {path: ".lefthook.jsonc", kind: lefthookKind}, {path: ".lefthook.toml", kind: lefthookKind},
	{path: ".config/lefthook.yml", kind: lefthookKind}, {path: ".config/lefthook.yaml", kind: lefthookKind},
	{path: ".config/lefthook.json", kind: lefthookKind}, {path: ".config/lefthook.jsonc", kind: lefthookKind}, {path: ".config/lefthook.toml", kind: lefthookKind},
	{path: ".pre-commit-config.yaml", kind: preCommitKind}, {path: ".pre-commit-config.yml", kind: preCommitKind},
	{path: "prek.toml", kind: prekKind}, {path: ".prek.toml", kind: prekKind},
}

// hkDiscoveryPaths are the files hk may load in place of hk.pkl. An inherited recheck runs hk at the upper tip, so
// an upper increment that adds one of these would change the config that runs. Refuse inheritance for each.
var hkDiscoveryPaths = []string{hkConfigName, "hk.local.pkl", ".config/hk.pkl", ".config/hk.local.pkl"}

// goBuildFiles are the module files that change what the gate runner builds. go.work and go.work.sum switch the
// workspace (GOWORK=auto), so an upper increment that adds one would change the build. go.mod and go.sum change the
// module graph.
var goBuildFiles = []string{"go.mod", "go.sum", "go.work", "go.work.sum"}

func (r *repository) validationBase(ctx context.Context, boundary, head string) (string, error) {
	return r.one(ctx, "heads(::"+quoteRev(boundary)+" & ::"+quoteRev(head)+")")
}

func (r *repository) hookConfigBlob(ctx context.Context, rev, path string) (string, error) {
	out, err := r.command(ctx, nil, nil, "git", "ls-tree", rev, "--", path)
	if err != nil {
		return "", err
	}
	if len(out) == 0 {
		return "", nil
	}
	fields := strings.Fields(string(out))
	if len(fields) < 4 || fields[1] != "blob" || !objectID.MatchString(fields[2]) {
		return "", errors.New("invalid hook config tree entry")
	}
	return fields[2], nil
}

func (r *repository) hookIdentity(ctx context.Context, change Change) (HookRecord, error) {
	rec, _, err := r.hookIdentityConfig(ctx, change)
	return rec, err
}

func (r *repository) hookIdentityConfig(ctx context.Context, change Change) (HookRecord, hookConfig, error) {
	base, err := r.validationBase(ctx, change.Base, change.Tip)
	if err != nil {
		return HookRecord{}, hookConfig{}, err
	}
	args := []string{"ls-tree", "-r", change.Tip, "--"}
	for _, config := range hookConfigs {
		args = append(args, config.path)
	}
	out, err := r.command(ctx, nil, nil, "git", args...)
	if err != nil {
		return HookRecord{}, hookConfig{}, err
	}
	blobs := map[string]string{}
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 4 || fields[1] != "blob" || !objectID.MatchString(fields[2]) {
			return HookRecord{}, hookConfig{}, errors.New("invalid hook config tree entry")
		}
		blobs[fields[3]] = fields[2]
	}
	for _, config := range hookConfigs {
		if blob := blobs[config.path]; blob != "" {
			return HookRecord{Repo: r.profile.Name, Head: change.Tip, Base: base, HookConfigBlob: blob}, config, nil
		}
	}
	return HookRecord{Repo: r.profile.Name, Head: change.Tip, Base: base}, hookConfig{}, nil
}

func sameSkips(a, b []SkippedTarget) bool {
	if len(a) != len(b) {
		return false
	}
	left, right := slices.Clone(a), slices.Clone(b)
	compare := func(x, y SkippedTarget) int {
		if n := strings.Compare(x.Target, y.Target); n != 0 {
			return n
		}
		return strings.Compare(x.Reason, y.Reason)
	}
	slices.SortFunc(left, compare)
	slices.SortFunc(right, compare)
	return slices.Equal(left, right)
}

func reusePartial(store localPrePush, rec HookRecord, skip []SkippedTarget) (bool, error) {
	history, err := store.History(rec.Repo, rec.Head)
	if err != nil {
		return false, err
	}
	for _, prior := range slices.Backward(history) {
		if prior.Result == hookPartial && prior.Current(rec.Repo, rec.Head, rec.Base, rec.HookConfigBlob) && sameSkips(prior.SkippedTargets, skip) {
			return true, store.Save(prior)
		}
	}
	return false, nil
}
func (r *repository) runSelectedHook(ctx context.Context, runner HookRunner, rec HookRecord, skip []SkippedTarget) (HookOutcome, RunRef, error) {
	if ssh, ok := runner.(sshHookRunner); ok && len(ssh.hosts) != 0 {
		picked, pick, err := ssh.pickRunnerHost(ctx, r.log)
		if err != nil {
			return HookOutcome{}, RunRef{}, err
		}
		defer picked.cleanupTickets(ctx)
		if _, err := fmt.Fprintln(r.log, runnerSelection(pick)); err != nil {
			return HookOutcome{}, RunRef{}, toolFailure("hooks", err)
		}
		if _, err := fmt.Fprintln(os.Stderr, runnerSelection(pick)); err != nil {
			return HookOutcome{}, RunRef{}, toolFailure("hooks", err)
		}
		runner = picked
	} else if _, ok := runner.(localHookRunner); ok {
		if _, err := fmt.Fprintln(r.log, runnerSelection(hostPick{})); err != nil {
			return HookOutcome{}, RunRef{}, toolFailure("hooks", err)
		}
	}
	return runner.Run(ctx, rec.Base, rec.Head, skip, r.log)
}

func (r *repository) runHooks(ctx context.Context, line []Change, skip []SkippedTarget, h HookRunner, store localPrePush) error {
	for _, change := range line {
		if !change.Owned {
			continue
		}
		rec, err := r.hookIdentity(ctx, change)
		if err != nil {
			return err
		}
		latest, found, err := store.Read(ctx, rec.Repo, 0, rec.Head)
		if err != nil {
			return err
		}
		if found && latest.Current(rec.Repo, rec.Head, rec.Base, rec.HookConfigBlob) && ((latest.Result == hookPass && len(skip) == 0) || (latest.Result == hookPartial && sameSkips(latest.SkippedTargets, skip))) {
			continue
		}
		if len(skip) != 0 {
			reused, err := reusePartial(store, rec, skip)
			if err != nil {
				return err
			}
			if reused {
				continue
			}
		}
		out, ref, err := r.runSelectedHook(ctx, h, rec, skip)
		if err != nil {
			return err
		}
		if out.Config != "" {
			blob, err := r.hookConfigBlob(ctx, rec.Head, out.Config)
			if err != nil {
				return err
			}
			if blob != rec.HookConfigBlob {
				return toolFailure("hooks", errors.New("hook config differs from detected config"))
			}
		}
		rec.At, rec.Run, rec.Digest = time.Now().UTC(), ref.ID, ref.Digest
		rec.FailedTargets, rec.FailedGates = out.FailedTargets, out.FailedGates
		rec.SkippedTargets = slices.Clone(skip)
		switch out.Phase {
		case phaseNoConfig:
			rec.Result = hookAbsent
		case phaseFixup:
			rec.Result, rec.Fixup = hookFixup, out.Fixup
		case phaseCompleted:
			switch {
			case out.Exit != 0:
				rec.Result = hookFail
			case len(skip) != 0:
				rec.Result = hookPartial
			default:
				rec.Result = hookPass
			}
		default:
			return toolFailure("hooks", fmt.Errorf("invalid hook phase %q", out.Phase))
		}
		if err := store.Save(rec); err != nil {
			return err
		}
		switch rec.Result {
		case hookAbsent:
			return refusal("hooks", "hooks-absent", "no hook configuration at head", "add a pre-push hook")
		case hookFail:
			return refusal("hooks", "hooks-failed", "pre-push hooks failed", "fix the failure and rerun rjc submit")
		case hookFixup:
			return refusal("hooks", "hooks-fixup", "hooks created a fixup commit", "jj new "+rec.Fixup+"; then rjc submit")
		case hookPass, hookPartial, hookSkipped, hookMainRed:
		}
	}
	return nil
}

func (r *repository) publishHooks(ctx context.Context, line []Change, store localPrePush) error {
	s, err := service(ctx)
	if err != nil {
		return err
	}
	for _, change := range line {
		if !change.Owned {
			continue
		}
		rec, ok, err := store.Read(ctx, r.profile.Name, 0, change.Tip)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("missing hook result")
		}
		pr, err := s.forge.findPR(ctx, r.profile.Name, change.Bookmark)
		if err != nil {
			return err
		}
		if pr == 0 {
			return errors.New("missing PR for hook result")
		}
		if err := store.Publish(ctx, r.profile.Name, pr, rec); err != nil {
			return err
		}
	}
	return nil
}
