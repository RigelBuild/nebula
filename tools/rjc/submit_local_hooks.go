package main

import (
	"context"
	"regexp"
	"strings"

	"github.com/RigelBuild/nebula/tools/rjc/internal/hookshell"
)

const localHooksRefuse = "refuse"

func (r *repository) refuseLocalHooks(fix string) error {
	if envLookup(r.env, "RJC_HOOK_LOCAL") != localHooksRefuse {
		return nil
	}
	return refusal(
		"hooks",
		"local-hooks-refused",
		"this machine permits only approved fast local hooks",
		fix,
	)
}

func (r *repository) preflightLocalHooks(ctx context.Context, target string, opt submitOptions, plan retargetPlan, reviewPlan *reviewFixPlan, top, selectionTip string) error {
	if !r.profile.Hooks || opt.skip != "" || envLookup(r.env, "RJC_HOOK_LOCAL") != localHooksRefuse {
		return nil
	}
	if _, local := r.hookRunner().(localHookRunner); !local {
		return nil
	}
	if !r.profile.LocalFastTier {
		return r.refuseNonFastLocalHooks(ctx, []Change{{Owned: true}})
	}
	trunk, err := r.one(ctx, r.profile.TrunkRemote())
	if err != nil {
		return err
	}
	if err := r.refuseNonFastLocalHooks(ctx, []Change{{Tip: trunk, Owned: true}}); err != nil {
		return err
	}
	if opt.at != "" {
		return r.refuseNonFastLocalHooks(ctx, plan.line)
	}
	if target == "" {
		tip, err := r.one(ctx, "@-")
		if err != nil {
			return err
		}
		return r.refuseNonFastLocalHooks(ctx, []Change{{Tip: tip, Owned: true}})
	}
	line, err := r.submitHookLine(ctx, target, top, selectionTip, reviewPlan)
	if err != nil {
		return err
	}
	return r.refuseNonFastLocalHooks(ctx, line)
}

var fastHookRE = regexp.MustCompile(`(?s)^\s*amends\s+"package://github\.com/jdx/hk/releases/download/v` + regexp.QuoteMeta(hookshell.HKVersion) + `/hk@` + regexp.QuoteMeta(hookshell.HKVersion) + `#/Config\.pkl"\s*default_branch\s*=\s*"main"\s*hooks\s*\{\s*\["pre-push"\]\s*\{\s*steps\s*\{\s*\["ci"\]\s*=\s*new\s+Step\s*\{\s*check\s*=\s*"go run \./tools/gate-runner moon-ci --tier fast"\s*\}\s*\["gates"\]\s*=\s*new\s+Step\s*\{\s*check\s*=\s*"go run \./tools/gate-runner"\s*depends\s*=\s*List\("ci"\)\s*\}\s*\}\s*\}\s*\}\s*$`)

func (r *repository) refuseNonFastLocalHooks(ctx context.Context, line []Change) error {
	if envLookup(r.env, "RJC_HOOK_LOCAL") != localHooksRefuse {
		return nil
	}
	if _, local := r.hookRunner().(localHookRunner); !local {
		return nil
	}
	fix := "export RJC_HOOK_SSH=user@host[,…] in the command before rjc submit; " +
		"do not set it only in a tool env: parameter"
	for _, change := range line {
		if !change.Owned {
			continue
		}
		if !r.profile.LocalFastTier {
			return r.refuseLocalHooks(fix)
		}
		for _, path := range []string{"hk.local.pkl", ".config/hk.local.pkl"} {
			blob, err := r.hookConfigBlob(ctx, change.Tip, path)
			if err != nil {
				return err
			}
			if blob != "" {
				return r.refuseLocalHooks(fix)
			}
		}
		blob, err := r.hookConfigBlob(ctx, change.Tip, hkConfigName)
		if err != nil {
			return err
		}
		if blob == "" {
			return r.refuseLocalHooks(fix)
		}
		content, err := r.command(ctx, nil, nil, "git", "cat-file", "blob", blob)
		if err != nil {
			return err
		}
		var config strings.Builder
		for line := range strings.SplitSeq(string(content), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "//") {
				config.WriteString(line)
				config.WriteByte('\n')
			}
		}
		if !fastHookRE.MatchString(config.String()) {
			return r.refuseLocalHooks(fix)
		}
	}
	return nil
}
