package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec" //nolint:depguard // Detect a missing linear binary via exec.ErrNotFound.
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var hashReason = regexp.MustCompile(`^#[1-9][0-9]*$`)
var targetName = regexp.MustCompile(`^[A-Za-z0-9_.-]+:[A-Za-z0-9_.-]+$`)

// skipReasonMax caps a free-text skip reason, counted in runes.
const skipReasonMax = 200

// skipReasonFix names the allowed --skip-checks forms in refusals.
const skipReasonFix = "supply a one-line reason of up to 200 characters, main-red:<ref>, or p0:<PREFIX>-<n>"

var mainRedSkip = regexp.MustCompile(`^main-red:\S+$`)

// p0SkipShape is the prefix-agnostic p0 grammar. validSkipReason then requires the repository's own prefix.
var p0SkipShape = regexp.MustCompile(`^p0:[A-Za-z][A-Za-z0-9]*-[1-9][0-9]*$`)

// validSkipReasonShape keeps the main-red and p0 forms exact and needs no issue prefix. Any other trimmed, one-line
// text up to skipReasonMax runes is a free-text reason. validSkipReason checks a p0 reason's prefix.
func validSkipReasonShape(skip string) bool {
	if strings.IndexFunc(skip, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
		return false
	}
	// A stack marker in the PR body breaks splitBody on every later submit.
	if strings.Contains(skip, stackStart) || strings.Contains(skip, stackEnd) {
		return false
	}
	switch {
	case strings.HasPrefix(skip, "p0:"):
		return p0SkipShape.MatchString(skip)
	case strings.HasPrefix(skip, "main-red:"):
		return mainRedSkip.MatchString(skip)
	}
	return strings.TrimSpace(skip) != "" && utf8.RuneCountInString(skip) <= skipReasonMax
}

// validSkipReason checks a skip reason against the issue prefix in keys.
func validSkipReason(keys issueKeys, skip string) bool {
	if !validSkipReasonShape(skip) {
		return false
	}
	if strings.HasPrefix(skip, "p0:") {
		return keys.p0SkipRE.MatchString(skip)
	}
	return true
}

func (r *repository) linearIssuePriority(ctx context.Context, issue string) (int, string, bool, error) {
	if r.lookupLinearPriority != nil {
		return r.lookupLinearPriority(ctx, issue)
	}
	query := fmt.Sprintf(`{ issue(id:%q){ priority state { type } } }`, issue)
	output, err := r.command(ctx, nil, nil, "linear", "api", query)
	var response struct {
		Data struct {
			Issue *struct {
				Priority *int `json:"priority"`
				State    *struct {
					Type string `json:"type"`
				} `json:"state"`
			} `json:"issue"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err != nil {
		if len(output) > 0 && json.Unmarshal(output, &response) == nil && len(response.Errors) > 0 && response.Errors[0].Message != "" {
			return 0, "", false, fmt.Errorf("linear API error: %s: %w", response.Errors[0].Message, err)
		}
		return 0, "", false, err
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return 0, "", false, fmt.Errorf("decode Linear issue status: %w", err)
	}
	if response.Data.Issue == nil || response.Data.Issue.Priority == nil || response.Data.Issue.State == nil {
		return 0, "", false, nil
	}
	return *response.Data.Issue.Priority, response.Data.Issue.State.Type, true, nil
}

func (r *repository) checkP0Priority(ctx context.Context, issue, stage, gate string) error {
	priority, state, found, err := r.linearIssuePriority(ctx, issue)
	if err != nil {
		fix := "check Linear access and retry"
		reason := "cannot verify issue priority"
		if errors.Is(err, exec.ErrNotFound) {
			reason = "linear CLI is not installed or is not on PATH"
			fix = "install the linear CLI and ensure it is on PATH"
		}
		return &failure{stage: stage, gate: gate, reason: reason + ": " + err.Error(), fix: fix, code: 1, cause: err}
	}
	if !found {
		return refusal(stage, gate, "P0 issue does not exist or has no priority/state", "check the Linear issue ID and retry")
	}
	if priority != 1 {
		return refusal(stage, gate, "P0 skip requires an Urgent issue", "use an Urgent-priority Linear issue")
	}
	if state == "completed" || state == "canceled" {
		return refusal(stage, gate, "P0 issue is closed", "use an open Urgent-priority Linear issue")
	}
	return nil
}

func (r *repository) checkReason(ctx context.Context, reason string) error {
	if r.keys.issueReasonRE.MatchString(reason) {
		return nil
	}
	number := ""
	if hashReason.MatchString(reason) {
		number = reason[1:]
	} else {
		u, err := url.Parse(reason)
		if err == nil && u.Scheme == "https" && u.Host == "github.com" && u.RawQuery == "" && u.Fragment == "" {
			parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
			repo := strings.Split(r.profile.Name, "/")
			if len(parts) == 4 && len(repo) == 2 && strings.EqualFold(parts[0], repo[0]) && strings.EqualFold(parts[1], repo[1]) && (parts[2] == "issues" || parts[2] == "pull") && hashReason.MatchString("#"+parts[3]) {
				number = parts[3]
			}
		}
	}
	if number == "" {
		return refusal("hooks", "skip-reason", "invalid or cross-repository skip reason", "supply an open issue or PR")
	}
	s, err := service(ctx)
	if err != nil {
		return err
	}
	var issue struct {
		State string `json:"state"`
	}
	if err := s.forge.api(ctx, "GET", "/repos/"+r.profile.Name+"/issues/"+number, nil, &issue); err != nil {
		var apiErr *forgeAPIError
		if errors.As(err, &apiErr) && apiErr.status == http.StatusNotFound {
			return refusal("hooks", "skip-reason", "skip reason does not exist", "supply an open issue or PR")
		}
		return err
	}
	if issue.State != "open" {
		return refusal("hooks", "skip-reason", "skip reason is not open", "supply an open issue or PR")
	}
	return nil
}

func (r *repository) checkSkip(ctx context.Context, line []Change, skip string, store localPrePush) error {
	skip = strings.TrimSpace(skip)
	if !validSkipReason(r.keys, skip) {
		return refusal("hooks", "skip-reason", "invalid skip reason", skipReasonFix)
	}
	p0Issue, p0 := strings.CutPrefix(skip, "p0:")
	if p0 {
		if err := r.checkP0Priority(ctx, p0Issue, "hooks", "skip-reason"); err != nil {
			return err
		}
	}
	ref, mainRed := strings.CutPrefix(skip, "main-red:")
	if mainRed {
		if r.keys.issueReasonRE.MatchString(ref) {
			return refusal("hooks", "skip-reason", "main-red requires a GitHub issue or PR", "supply an open issue or PR")
		}
		if err := r.checkReason(ctx, ref); err != nil {
			return err
		}
	}
	records := []HookRecord{}
	var failed []failedHead
	for _, change := range line {
		if !change.Owned {
			continue
		}
		id, err := r.hookIdentity(ctx, change)
		if err != nil {
			return err
		}
		// Free-text and P0 skips need no failed run; main-red does.
		if !mainRed {
			id.Result, id.Skip, id.At = hookSkipped, skip, time.Now().UTC()
			records = append(records, id)
			continue
		}
		fail, found, err := currentFailure(store, id)
		if err != nil {
			return err
		}
		if found {
			failed = append(failed, failedHead{change: change, fail: fail})
		} else {
			// Hooks stop at the first failing head, so a head above it may have no run of its own.
			// A head that ran keeps its own result, and only a head with no run may inherit.
			ran, err := headRan(ctx, store, id)
			if err != nil {
				return err
			}
			if ran {
				return refusal("hooks", "skip-evidence", "head has its own hook run that is not a current failure", "run hooks normally first")
			}
			lower, covered, err := r.coveringFailure(ctx, failed, change, id)
			if err != nil {
				return err
			}
			if !covered {
				return refusal("hooks", "skip-evidence", "no current failed hook run", "run hooks normally first")
			}
			if err := r.logInheritedFailure(change, lower); err != nil {
				return err
			}
			fail, id.CoveredBy = lower.fail, lower.change.Tip
		}
		id.Result, id.Skip, id.FailedRun, id.At = hookSkipped, skip, fail.Run, time.Now().UTC()
		id.FailedTargets, id.FailedGates = fail.FailedTargets, fail.FailedGates
		records = append(records, id)
	}
	for _, rec := range records {
		if err := store.Save(rec); err != nil {
			return err
		}
	}
	return nil
}

type failedHead struct {
	change Change
	fail   HookRecord
}

// currentFailure returns the newest failed hook run recorded for the head, if that run is still current.
func currentFailure(store localPrePush, id HookRecord) (HookRecord, bool, error) {
	history, err := store.History(id.Repo, id.Head)
	if err != nil {
		return HookRecord{}, false, err
	}
	for _, fail := range slices.Backward(history) {
		if fail.Result == hookFail && len(fail.SkippedTargets) == 0 && fail.Current(id.Repo, id.Head, id.Base, id.HookConfigBlob) {
			return fail, true, nil
		}
	}
	return HookRecord{}, false, nil
}

// headRan reports whether the head has a hook run of its own. Inherited records (CoveredBy set) are the
// lower head's evidence, not this head's run, so they never count.
func headRan(ctx context.Context, store localPrePush, id HookRecord) (bool, error) {
	history, err := store.History(id.Repo, id.Head)
	if err != nil {
		return false, err
	}
	for _, rec := range history {
		if rec.CoveredBy == "" {
			return true, nil
		}
	}
	latest, found, err := store.Read(ctx, id.Repo, 0, id.Head)
	if err != nil {
		return false, err
	}
	return found && latest.CoveredBy == "" && latest.Result != hookSkipped, nil
}

// coveringFailure returns the current failed run of a lower owned head that change descends from. The lower
// head must still be the only head of its bookmark, and change must descend from its tip. Its hook config
// blob must equal change's, so the inherited run tested the same hooks.
func (r *repository) coveringFailure(ctx context.Context, failed []failedHead, change Change, id HookRecord) (failedHead, bool, error) {
	for _, lower := range slices.Backward(failed) {
		heads, err := r.revisions(ctx, "bookmarks(exact:"+quoteRev(lower.change.Bookmark)+")")
		if err != nil {
			return failedHead{}, false, err
		}
		if len(heads) != 1 || heads[0] != lower.change.Tip {
			continue
		}
		covered, err := r.revisions(ctx, quoteRev(lower.change.Tip)+" & ::"+quoteRev(change.Tip))
		if err != nil {
			return failedHead{}, false, err
		}
		if len(covered) == 0 {
			continue
		}
		if lower.fail.HookConfigBlob != id.HookConfigBlob {
			return failedHead{}, false, refusal("hooks", "skip-evidence", "hook config of "+change.Bookmark+" differs from failed lower head "+lower.change.Bookmark, "run hooks on "+change.Bookmark+" first")
		}
		if err := r.refuseRiskyIncrement(ctx, lower.change.Tip, change.Tip); err != nil {
			return failedHead{}, false, err
		}
		return lower, true, nil
	}
	return failedHead{}, false, nil
}

// refuseRiskyIncrement refuses inheritance when the upper head's own increment could change what the recheck tests:
// an hk discovery path, a hook config, go.mod or go.sum, or a file in the gate-runner build closure. The closure is
// computed at the lower tip, which is already validated, so the untrusted upper tree is never executed. An upper
// increment that adds a package must first edit a file already in that closure, and that edit is refused here.
func (r *repository) refuseRiskyIncrement(ctx context.Context, lowerTip, upperTip string) error {
	closure, err := r.gateRunnerClosure(ctx, lowerTip)
	if err != nil {
		return refusal("hooks", "skip-evidence", "cannot derive the gate-runner closure, so inheritance is refused: "+err.Error(), "run hooks normally on this head")
	}
	paths, err := r.changedPaths(ctx, lowerTip, upperTip)
	if err != nil {
		return err
	}
	for _, path := range paths {
		if riskyPath(path, closure) {
			return refusal("hooks", "skip-evidence", "increment touches the gate runner, its build closure, or a hook config: "+path, "run hooks normally on this head")
		}
	}
	return nil
}

// riskyPath reports whether path is an hk discovery path, a hook config, go.mod, go.sum, go.work, go.work.sum, a
// vendor/ path at any depth, or inside a closure dir. go.work and vendor/ change what `go run ./tools/gate-runner`
// builds: GOWORK=auto reads a root go.work, and a vendor/ tree is used when present.
func riskyPath(path string, closure []string) bool {
	p := foldPath(path)
	if slices.ContainsFunc(hkDiscoveryPaths, func(s string) bool { return foldPath(s) == p }) ||
		slices.ContainsFunc(goBuildFiles, func(s string) bool { return foldPath(s) == p }) ||
		slices.Contains(strings.Split(p, "/"), "vendor") {
		return true
	}
	if slices.ContainsFunc(hookConfigs, func(config hookConfig) bool { return foldPath(config.path) == p }) {
		return true
	}
	for _, dir := range closure {
		if d := foldPath(dir); p == d || strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	return false
}

// foldPath is the form riskyPath compares paths in. macOS APFS and Windows NTFS match names case-insensitively,
// so an upper increment that adds GO.WORK or Vendor/x reaches Go as go.work or vendor/x.
func foldPath(path string) string { return strings.ToLower(path) }

// gateRunnerClosure lists the repository-internal package directories that the gate runner builds against, at rev. It
// runs go with GOTOOLCHAIN=local and -mod=readonly so go cannot fetch a toolchain or modify go.mod.
func (r *repository) gateRunnerClosure(ctx context.Context, rev string) (dirs []string, resultErr error) {
	dir, err := os.MkdirTemp("", "rjc-gate-closure-")
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(dir)) }()
	path := filepath.Join(dir, "workspace")
	name := filepath.Base(dir)
	if _, err = r.jj(ctx, "workspace", "add", "--name", name, "--revision", rev, path); err != nil {
		return nil, err
	}
	defer func() {
		_, cleanupErr := r.jj(context.WithoutCancel(ctx), "workspace", "forget", name)
		resultErr = errors.Join(resultErr, cleanupErr)
	}()
	workspace := *r
	workspace.root = path
	out, err := workspace.command(ctx, []string{"GOTOOLCHAIN=local", "GOFLAGS=-mod=readonly"}, nil, "go", "list", "-deps", "-f", "{{if .Module}}{{if .Module.Main}}{{.Dir}}{{end}}{{end}}", "./"+r.profile.GateRunnerDir)
	if err != nil {
		return nil, err
	}
	// go list reports resolved directories. Resolve the root the same way, or a symlinked TMPDIR (macOS /var ->
	// /private/var) makes every closure dir look outside the repository.
	root, err := filepath.EvalSymlinks(workspace.root)
	if err != nil {
		return nil, err
	}
	for pkg := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if pkg == "" {
			continue
		}
		rel, err := filepath.Rel(root, pkg)
		if err != nil || strings.HasPrefix(rel, "..") {
			return nil, fmt.Errorf("closure package %s is outside the repository", pkg)
		}
		dirs = append(dirs, filepath.ToSlash(rel))
	}
	if len(dirs) == 0 {
		return nil, errors.New("gate-runner closure is empty")
	}
	return dirs, nil
}

// logInheritedFailure names the lower head and run that cover an upper head, so the log shows inherited evidence.
func (r *repository) logInheritedFailure(change Change, lower failedHead) error {
	if _, err := fmt.Fprintf(r.log, "skip-evidence: %s inherits failed run %s from %s at %s\n", change.Tip, lower.fail.Run, lower.change.Bookmark, lower.change.Tip); err != nil {
		return toolFailure("hooks", err)
	}
	return nil
}

func (r *repository) checkSkipTargets(ctx context.Context, line []Change, skip []SkippedTarget, store localPrePush) error {
	if len(skip) > 0 && !r.profile.HookReports {
		return refusal("hooks", "hook-reports-off", "selective skips require hook reports", "run without --skip-target")
	}
	for _, target := range skip {
		if !targetName.MatchString(target.Target) {
			return refusal("hooks", "skip-target-not-failed", "invalid moon target", "supply a project:task target")
		}
		if err := r.checkReason(ctx, target.Reason); err != nil {
			return err
		}
	}
	for _, change := range line {
		if !change.Owned {
			continue
		}
		id, err := r.hookIdentity(ctx, change)
		if err != nil {
			return err
		}
		history, err := store.History(id.Repo, id.Head)
		if err != nil {
			return err
		}
		authorized := map[string]bool{}
		for _, rec := range history {
			if !rec.Current(id.Repo, id.Head, id.Base, id.HookConfigBlob) {
				continue
			}
			if rec.Result == hookFail {
				for _, target := range rec.FailedTargets {
					authorized[target] = true
				}
			}
			if rec.Result == hookFail || rec.Result == hookPartial {
				for _, target := range rec.SkippedTargets {
					authorized[target.Target] = true
				}
			}
		}
		for _, target := range skip {
			if !authorized[target.Target] {
				return refusal("hooks", "skip-target-not-failed", target.Target+" has no failed evidence", "run hooks without this skip")
			}
		}
	}
	return nil
}
