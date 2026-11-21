package main

import (
	"context"
	"strings"
)

func (r *repository) adoptOwnedRemote(ctx context.Context, bookmark string) (string, error) {
	if !r.keys.Owned(r.codename, bookmark) {
		return "", nil
	}
	listing, err := r.jj(ctx, "bookmark", "list", "exact:"+bookmark, "-T", `if(!remote, if(conflict, "conflict", "normal") ++ "\n" ++ added_targets.map(|c| c.commit_id()).join("\n"))`)
	if err != nil {
		return "", err
	}
	if listing == "" {
		return "", nil
	}
	parts := strings.Split(listing, "\n")
	if len(parts) < 2 {
		return "", refusal("gate-state", "adopt-ambiguous", "local bookmark has no target: "+bookmark, "resolve the bookmark before submitting")
	}
	conflicted := parts[0] == "conflict"
	local := parts[1:]
	if len(local) > 2 {
		return "", refusal("gate-state", "adopt-ambiguous", "local bookmark has more than two targets: "+bookmark, "resolve the bookmark before submitting")
	}
	untracked, err := r.revisions(ctx, "untracked_remote_bookmarks(exact:"+quoteRev(bookmark)+", remote=exact:\"origin\")")
	if err != nil {
		return "", err
	}
	untrackedRemote := len(untracked) > 0
	if !conflicted && !untrackedRemote {
		return "", nil
	}
	remote, err := r.revisions(ctx, "remote_bookmarks(exact:"+quoteRev(bookmark)+", remote=exact:\"origin\")")
	if err != nil {
		return "", err
	}
	if len(remote) != 1 {
		return "", refusal("gate-state", "adopt-ambiguous", "origin bookmark does not have one target: "+bookmark, "resolve the origin bookmark before submitting")
	}
	pr, metadata, err := r.publishedHead(ctx, bookmark)
	if err != nil {
		return "", err
	}
	if remote[0] != metadata.HeadSHA {
		return "", refusal("gate-state", "adopt-head-mismatch", "origin bookmark is not the open PR's head: "+bookmark, "inspect the origin bookmark and PR before submitting")
	}
	entry, found, err := (Ledger{root: r.stateDir}).findForPR(r.profile.Name, bookmark, pr)
	if err != nil {
		return "", err
	}
	if found && entry.PR == pr && entry.LastPushedHead == "" {
		return "", refusal("gate-state", "adopt-no-ledger-head", "this seat's ledger has no last-pushed head: "+bookmark, "verify the PR head with gh pr view, run jj bookmark track "+bookmark+"@origin, then submit")
	}
	if !found || entry.PR != pr || remote[0] != entry.LastPushedHead {
		return "", refusal("gate-state", "adopt-not-last-pushed", "origin does not match this seat's recorded last-pushed head: "+bookmark, "submit from the seat that last pushed this PR, or resolve the bookmark manually")
	}
	tip, err := adoptionTip(local, metadata.HeadSHA, bookmark)
	if err != nil {
		return "", err
	}
	if err := r.validateAdoptionTip(ctx, tip, metadata.HeadSHA, bookmark, conflicted); err != nil {
		return "", err
	}
	return tip, nil
}

func (r *repository) applyOwnedAdoption(ctx context.Context, bookmark, tip string) error {
	untracked, err := r.revisions(ctx, "untracked_remote_bookmarks(exact:"+quoteRev(bookmark)+", remote=exact:\"origin\")")
	if err != nil {
		return err
	}
	if len(untracked) > 0 {
		if _, err := r.jj(ctx, "bookmark", "track", bookmark+"@origin"); err != nil {
			return err
		}
	}
	_, err = r.jj(ctx, "bookmark", "set", bookmark, "-r", tip, "--allow-backwards")
	return err
}

// publishedHead returns the open PR number and head metadata for bookmark.
func (r *repository) publishedHead(ctx context.Context, bookmark string) (int, Metadata, error) {
	s, err := service(ctx)
	if err != nil {
		return 0, Metadata{}, err
	}
	pr, err := s.forge.findPR(ctx, r.profile.Name, bookmark)
	if err != nil {
		return 0, Metadata{}, err
	}
	if pr == 0 {
		return 0, Metadata{}, refusal("gate-state", "adopt-unpublished", "origin bookmark is non-tracking or conflicted and no open PR records its head: "+bookmark, "open the PR from the published head, or resolve the origin bookmark, then rerun rjc submit")
	}
	metadata, err := s.forge.metadata(ctx, r.profile.Name, pr)
	if err != nil {
		return 0, Metadata{}, err
	}
	if metadata.HeadRef != bookmark {
		return 0, Metadata{}, refusal("gate-state", "adopt-head-mismatch", "PR head ref differs from the owned bookmark: "+bookmark, "resolve the PR identity before submitting")
	}
	return pr, metadata, nil
}

// adoptionTip returns the sole local target other than the published head.
func adoptionTip(local []string, head, bookmark string) (string, error) {
	var others []string
	for _, id := range local {
		if id != head {
			others = append(others, id)
		}
	}
	switch len(others) {
	case 0:
		return head, nil
	case 1:
		return others[0], nil
	default:
		return "", refusal("gate-state", "adopt-ambiguous", "local bookmark has more than one target besides the PR head: "+bookmark, "set the local bookmark to the commit to submit, then rerun rjc submit")
	}
}

func (r *repository) validateAdoptionTip(ctx context.Context, tip, head, bookmark string, conflicted bool) error {
	if tip == head {
		return nil
	}
	backwards, err := r.revisions(ctx, quoteRev(tip)+" & ::"+quoteRev(head))
	if err != nil {
		return err
	}
	if len(backwards) > 0 {
		return refusal("gate-state", "adopt-backwards", "local bookmark target is an ancestor of the pushed PR head: "+bookmark, "restore the pushed head before submitting")
	}
	if !conflicted {
		return nil
	}
	headChange, err := r.jj(ctx, "log", "--no-graph", "-r", quoteRev(head), "-T", "change_id")
	if err != nil {
		return err
	}
	tipChange, err := r.jj(ctx, "log", "--no-graph", "-r", quoteRev(tip), "-T", "change_id")
	if err != nil {
		return err
	}
	if headChange == tipChange {
		return nil
	}
	return refusal("gate-state", "adopt-foreign-rewrite", "conflicted local bookmark target is not a rewrite of the pushed head: "+bookmark, "resolve the bookmark conflict before submitting")
}
