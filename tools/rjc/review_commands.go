package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type noteOptions struct {
	bookmark, round, file, reviewed string
	clear                           bool
}
type readyOptions struct {
	bookmark string
	mainRed  bool
}

//nolint:cyclop // note has one flag-validation block and one posting path; the branches share the fetched PR state.
func (c *cli) noteCommand() (*cobra.Command, error) {
	var opt noteOptions
	cmd := &cobra.Command{Use: "note [bookmark]", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		roundSelected := opt.round != ""
		invalidSelection := (roundSelected && opt.clear) || (!roundSelected && !opt.clear)
		fileWithoutClear := !opt.clear && opt.file != ""
		if invalidSelection || !objectID.MatchString(opt.reviewed) || fileWithoutClear {
			return &failure{stage: usageStage, gate: "note", reason: "specify --round or --clear and a 40-hex --reviewed", code: 2}
		}
		if len(args) > 0 {
			opt.bookmark = args[0]
		}
		ctx, r, err := c.initialize(cmd.Context())
		if err != nil {
			return err
		}
		if err = prepareForge(ctx, r); err != nil {
			return toolFailure("preflight", err)
		}
		bookmark, entry, meta, change, err := r.reviewPR(ctx, opt.bookmark)
		if err != nil {
			return err
		}
		s, err := service(ctx)
		if err != nil {
			return err
		}
		url := ""
		if opt.clear {
			if err = r.checkReviewHead(ctx, bookmark, meta, change, opt.reviewed); err != nil {
				return err
			}
			viewer, err := s.forge.viewer(ctx)
			if err != nil {
				return toolFailure("note", err)
			}
			markers, err := s.forge.comments(ctx, r.profile.Name, entry.PR)
			if err != nil {
				return toolFailure("note", err)
			}
			for _, m := range markers {
				if m.Kind == markerAllClear && m.Reviewed == opt.reviewed && m.Base == meta.BaseRef && m.Author == viewer {
					url = m.URL
					break
				}
			}
		} else {
			revs, err := r.revisions(ctx, quoteRev(opt.reviewed)+" & ::"+quoteRev(bookmark))
			if err != nil {
				return err
			}
			if len(revs) != 1 {
				return refusal("note", "not-ancestor", "reviewed commit is not an ancestor of the bookmark", "review an ancestor")
			}
		}
		file := opt.round
		kind := markerRound
		text := ""
		if opt.clear {
			file = opt.file
			kind = markerAllClear
			text = allClearHeader(opt.reviewed, meta.BaseRef)
		}
		if file != "" && url == "" {
			b, err := os.ReadFile(file) //nolint:gosec // The caller explicitly supplies the review file.
			if err != nil {
				return toolFailure("note", err)
			}
			if !opt.clear && strings.Contains(string(b), "<!-- rjc:") {
				return refusal("note", "marker-in-round", "round file contains an rjc marker", "remove the marker from the round file")
			}
			text += string(b) + "\n\n"
		}
		if url == "" {
			text += FormatMarker(Marker{Kind: kind, Reviewed: opt.reviewed, Base: meta.BaseRef})
			url, err = s.forge.postComment(ctx, r.profile.Name, entry.PR, text)
			if err != nil {
				return toolFailure("note", err)
			}
		}
		c.message = url
		if opt.clear && meta.Draft {
			if err := c.promoteNotedDraft(ctx, r, bookmark, url); err != nil {
				return err
			}
		}
		return nil
	}}
	cmd.Flags().StringVar(&opt.round, "round", "", "File containing the review round.")
	cmd.Flags().StringVar(&opt.reviewed, "reviewed", "", "Reviewed commit SHA.")
	cmd.Flags().BoolVar(&opt.clear, "clear", false, "Post an all-clear.")
	cmd.Flags().StringVar(&opt.file, "file", "", "Optional all-clear file.")
	return cmd, nil
}

// allClearHeader is the visible part of an all-clear note. The merge bot reads
// only the first non-blank line, and it must fully match the bot's all-clear
// pattern. The base goes on its own line so it
// does not change that first line.
func allClearHeader(reviewed, base string) string {
	return fmt.Sprintf("All-clear at %s\nBase: %s\n\n", reviewed, base)
}

func (c *cli) promoteNotedDraft(ctx context.Context, r *repository, bookmark, url string) error {
	// Re-read the PR and comments to catch a moved head before promotion.
	if err := c.promoteReady(ctx, r, bookmark, false); err != nil {
		refused, ok := errors.AsType[*failure](err)
		if ok {
			refused.reason = "all-clear posted at " + url + "; PR left as draft: " + refused.reason
			return err
		}
		return toolFailure("note", fmt.Errorf("all-clear posted at %s; PR left as draft: %w", url, err))
	}
	c.message = url + "; " + c.message
	return nil
}

func (c *cli) readyCommand() (*cobra.Command, error) {
	var opt readyOptions
	cmd := &cobra.Command{Use: "ready [bookmark]", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			opt.bookmark = args[0]
		}
		ctx, r, err := c.initialize(cmd.Context())
		if err != nil {
			return err
		}
		if err = prepareForge(ctx, r); err != nil {
			return toolFailure("preflight", err)
		}
		return c.promoteReady(ctx, r, opt.bookmark, opt.mainRed)
	}}

	cmd.Flags().BoolVar(&opt.mainRed, "main-red", false, "Recheck skipped work on the trunk.")
	return cmd, nil
}

func (c *cli) promoteReady(ctx context.Context, r *repository, bookmark string, mainRed bool) error {
	bookmark, entry, meta, change, err := r.reviewPR(ctx, bookmark)
	if err != nil {
		return err
	}
	if err = r.checkReviewHead(ctx, bookmark, meta, change, meta.HeadSHA); err != nil {
		return err
	}
	s, err := service(ctx)
	if err != nil {
		return err
	}
	viewer, err := s.forge.viewer(ctx)
	if err != nil {
		return toolFailure("ready", err)
	}
	markers, err := s.forge.comments(ctx, r.profile.Name, entry.PR)
	if err != nil {
		return toolFailure("ready", err)
	}
	if !hasAllClear(markers, viewer, meta) {
		return refusal("ready", "all-clear", "no current all-clear from the token account", "run the review loop")
	}
	store := localPrePush{root: r.stateDir}
	rec, found, err := store.Read(ctx, r.profile.Name, entry.PR, meta.HeadSHA)
	if err != nil {
		return toolFailure("ready", err)
	}
	identity, err := r.hookIdentity(ctx, change)
	if err != nil {
		return toolFailure("ready", err)
	}
	if !found || !rec.Current(identity.Repo, identity.Head, identity.Base, identity.HookConfigBlob) {
		return refusal("ready", "hooks-not-passed", "no current hook evidence", "rjc submit")
	}
	if err := r.checkReadyHooks(ctx, store, rec, mainRed); err != nil {
		return err
	}
	if meta.Draft {
		if err = s.forge.markReady(ctx, r.profile.Name, entry.PR); err != nil {
			return toolFailure("ready", err)
		}
	} else {
		c.message = "already-ready; "
	}
	c.message += "ready; hand off to the merge owner"
	return nil
}

func (r *repository) checkReadyHooks(ctx context.Context, store localPrePush, rec HookRecord, mainRed bool) error {
	if mainRed && rec.Result != hookPass {
		var err error
		rec, err = r.recordMainRed(ctx, store, rec)
		if err != nil {
			return err
		}
	}
	switch rec.Result {
	case hookPass:
		return nil
	case hookMainRed:
		tip, err := r.one(ctx, quoteRev(r.profile.TrunkRemote()))
		if err != nil {
			return err
		}
		if tip != rec.MainTip {
			return refusal("ready", "hooks-not-passed", "trunk moved since the red-main recheck", "rjc ready --main-red")
		}
		return nil
	case hookSkipped:
		if issue, ok := strings.CutPrefix(rec.Skip, "p0:"); ok {
			return r.checkP0Priority(ctx, issue, "ready", "hooks-skipped")
		}
		// Free-text skips promote with plain ready; main-red skips need --main-red.
		if strings.HasPrefix(rec.Skip, "main-red:") {
			return skippedReady(rec)
		}
		return nil
	case hookPartial:
		return skippedReady(rec)
	default:
		return refusal("ready", "hooks-not-passed", "hooks did not pass", "rjc submit")
	}
}

func hasAllClear(markers []Marker, viewer string, meta Metadata) bool {
	for _, m := range markers {
		if m.Kind == markerAllClear && m.Author == viewer && m.Reviewed == meta.HeadSHA && m.Base == meta.BaseRef {
			return true
		}
	}
	return false
}

func skippedReady(rec HookRecord) error {
	fix := "run the hooks without skipping"
	if canRecheck(rec) {
		fix = "rjc ready --main-red"
	}
	return refusal("ready", "hooks-skipped", "hooks were skipped", fix)
}

func canRecheck(rec HookRecord) bool {
	_, _, _, err := leftOut(rec) //nolint:dogsled // Only eligibility matters here.
	return err == nil
}

func (r *repository) recordMainRed(ctx context.Context, store localPrePush, rec HookRecord) (HookRecord, error) {
	if _, _, _, err := leftOut(rec); err != nil {
		return rec, err
	}
	if err := r.refuseLocalHooks(
		"run rjc ready --main-red on a machine that allows full local runs (RJC_HOOK_LOCAL unset)",
	); err != nil {
		return rec, err
	}
	if ref, ok := strings.CutPrefix(rec.Skip, "main-red:"); ok {
		if err := r.checkReason(ctx, ref); err != nil {
			return rec, err
		}
	}
	tip, err := r.one(ctx, quoteRev(r.profile.TrunkRemote()))
	if err != nil {
		return rec, err
	}
	passed, err := r.recheckMainRed(ctx, r.profile, rec, tip)
	if err != nil {
		return rec, toolFailure("ready", err)
	}
	if len(passed) > 0 {
		return rec, refusal("ready", "not-red-on-main", "work passed on trunk: "+strings.Join(passed, ", "), "fix the head")
	}
	rec.Result, rec.MainTip, rec.At = hookMainRed, tip, time.Now()
	if err := store.Save(rec); err != nil {
		return rec, toolFailure("ready", err)
	}
	return rec, nil
}

func (r *repository) reviewPR(ctx context.Context, bookmark string) (string, LedgerEntry, Metadata, Change, error) {
	if err := r.fetchMain(ctx); err != nil {
		return "", LedgerEntry{}, Metadata{}, Change{}, err
	}
	if bookmark == "" {
		names, err := r.localBookmarks(ctx, quoteRev(r.profile.TrunkRemote())+"..@-")
		if err != nil {
			return "", LedgerEntry{}, Metadata{}, Change{}, err
		}
		for _, name := range names {
			if r.keys.Owned(r.codename, name) {
				bookmark = name
			}
		}
	}
	if !r.keys.Owned(r.codename, bookmark) {
		return "", LedgerEntry{}, Metadata{}, Change{}, refusal("ownership", "foreign-bookmark", "no owned bookmark selected", "select an owned bookmark")
	}
	entry, ok, err := (Ledger{root: r.stateDir}).Find(r.profile.Name, bookmark)
	if err != nil {
		return "", entry, Metadata{}, Change{}, err
	}
	if !ok || entry.Codename != r.codename {
		return "", entry, Metadata{}, Change{}, refusal("ownership", "not-in-ledger", "bookmark has no owned ledger entry", "submit the bookmark")
	}
	s, err := service(ctx)
	if err != nil {
		return "", entry, Metadata{}, Change{}, err
	}
	pr, err := s.forge.findPR(ctx, r.profile.Name, bookmark)
	if err != nil {
		return "", entry, Metadata{}, Change{}, err
	}
	if pr != entry.PR {
		return "", entry, Metadata{}, Change{}, refusal("ready", "pr-not-open", "ledger PR is not open", "submit an open PR")
	}
	meta, err := s.forge.metadata(ctx, r.profile.Name, entry.PR)
	if err != nil {
		return "", entry, meta, Change{}, err
	}
	// note and ready rewrite nothing: skip rewrite-safety checks only.
	// checkReviewHead pins the one reviewed head.
	line, err := r.line(ctx, bookmark, lineOptions{reviewOnly: true})
	if err != nil {
		return "", entry, meta, Change{}, err
	}
	return bookmark, entry, meta, line[len(line)-1], nil
}

func (r *repository) checkReviewHead(ctx context.Context, bookmark string, meta Metadata, change Change, reviewed string) error {
	remote, err := r.one(ctx, quoteRev(bookmark+"@origin"))
	if err != nil {
		return err
	}
	if reviewed != meta.HeadSHA || reviewed != change.Tip || reviewed != remote || meta.HeadRef != bookmark {
		return refusal("ready", "head-mismatch", "PR head, local tip and remote bookmark differ", "fetch and submit the current head")
	}
	if strings.TrimSuffix(change.Base, "@origin") != meta.BaseRef {
		if r.profile.Posture == forkTracking && meta.BaseRef == r.profile.Mirror {
			return refusal("ready", "fork-base-mismatch", "tracking fork PR targets the mirror", "rjc sync, then rjc submit --retarget")
		}
		return refusal("ready", "base-mismatch", "PR base differs from the validated line", "sync and submit")
	}
	return nil
}
