package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

type retireOptions struct {
	bookmark string
	noPR     bool
}

func retirementPlanValid(plan, bookmark string) bool {
	count := 0
	for line := range strings.SplitSeq(plan, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "bookmark: ") {
			fields := strings.Fields(line)
			if len(fields) != 5 || fields[1] != bookmark || fields[2] != "[delete" || fields[3] != "from" || !strings.HasSuffix(fields[4], "]") {
				return false
			}
			count++
		}
	}
	return count == 1
}

func (r *repository) retire(ctx context.Context, opt retireOptions, log io.Writer) error {
	if !r.keys.Owned(r.codename, opt.bookmark) {
		return refusal("retire", "foreign-bookmark", "bookmark is not owned", "select an owned bookmark")
	}
	s, err := service(ctx)
	if err != nil {
		return err
	}
	ledger := Ledger{root: r.stateDir}
	entry, found, err := ledger.Find(r.profile.Name, opt.bookmark)
	if err != nil {
		return err
	}
	if found && entry.Codename != r.codename {
		return refusal("retire", "foreign-bookmark", "ledger entry belongs to another caller", "select an owned bookmark")
	}
	if opt.noPR {
		owner, _, _ := strings.Cut(r.profile.Name, "/")
		var prs []struct {
			Number int
			Head   struct{ Ref string }
		}
		path := "/repos/" + r.profile.Name + "/pulls?state=all&head=" + url.QueryEscape(owner+":"+opt.bookmark) + "&per_page=100"
		if err := s.forge.api(ctx, http.MethodGet, path, nil, &prs); err != nil {
			return err
		}
		if found || len(prs) > 0 {
			return refusal("retire", "pr-exists", "bookmark has a PR", "retire without --no-pr after it closes")
		}
	} else {
		if !found {
			return refusal("retire", "not-in-ledger", "bookmark has no ledger entry", "use --no-pr only if no PR exists")
		}
		var pr struct {
			State string
			Head  struct{ Ref string }
		}
		if err := s.forge.api(ctx, http.MethodGet, prPath(r.profile.Name, entry.PR), nil, &pr); err != nil {
			return err
		}
		if pr.State != prStateClosed || pr.Head.Ref != opt.bookmark {
			return refusal("retire", "pr-open", "PR is not closed or has a different head", "close or merge the PR first")
		}
	}
	tip, err := r.one(ctx, quoteRev(opt.bookmark))
	if err != nil {
		return err
	}
	remote, err := r.revisions(ctx, "present("+quoteRev(opt.bookmark+"@origin")+")")
	if err != nil {
		return err
	}
	if _, err := r.jj(ctx, "bookmark", "delete", opt.bookmark); err != nil {
		return err
	}
	restore := func(cause error) error {
		_, restoreErr := r.jj(ctx, "bookmark", "set", opt.bookmark, "-r", tip, "--allow-backwards")
		if restoreErr != nil {
			return errors.Join(cause, fmt.Errorf("restore local bookmark: %w", restoreErr))
		}
		return cause
	}
	if len(remote) > 0 {
		args := []string{"--no-pager", "git", "push", "--remote", originRemote, "--bookmark", "exact:" + opt.bookmark, "--dry-run"}
		cmd := r.execCommand(ctx, "jj", args...)
		cmd.Dir = r.root
		cmd.Env = r.childEnv(nil)
		output, err := cmd.CombinedOutput()
		if log != nil {
			if _, writeErr := fmt.Fprintln(log, string(output)); writeErr != nil {
				return restore(writeErr)
			}
		}
		if err != nil {
			return restore(refusal("retire", "push-plan", err.Error(), "inspect the remote and retry"))
		}
		if !retirementPlanValid(string(output), opt.bookmark) {
			return restore(refusal("retire", "push-plan", "push plan does not delete exactly the selected bookmark", "inspect the remote and retry"))
		}
		if _, err := r.jj(ctx, "git", "push", "--remote", originRemote, "--bookmark", "exact:"+opt.bookmark); err != nil {
			return restore(refusal("retire", "push-failed", err.Error(), "retry retirement"))
		}
	}
	var branch any
	err = s.forge.api(ctx, http.MethodGet, "/repos/"+r.profile.Name+"/branches/"+url.PathEscape(opt.bookmark), nil, &branch)
	var apiErr *forgeAPIError
	if err == nil {
		return restore(refusal("retire", "branch-exists", "forge still reports the branch", "retry retirement"))
	}
	if !errors.As(err, &apiErr) || apiErr.status != http.StatusNotFound || apiErr.message != "Branch not found" {
		return restore(refusal("retire", "branch-unconfirmed", err.Error(), "retry retirement"))
	}
	return r.removeRetiredState(opt.bookmark, ledger, entry, found)
}

func (r *repository) removeRetiredState(bookmark string, ledger Ledger, entry LedgerEntry, found bool) error {
	if found {
		path, err := ledger.path(entry.Repo, entry.PR)
		if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for _, suffix := range []string{".published.md", ".candidate.md", ".metadata.json", ".parent.json", ".upstream.json"} {
		path, err := (fileBaseline{root: r.stateDir, keys: r.keys}).path(bookmark, suffix)
		if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
