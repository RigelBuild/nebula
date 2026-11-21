package main

import (
	"context"
	"strings"
)

func (r *repository) checkEngineSelection(ctx context.Context, change Change, remoteBase bool) error {
	head, err := r.one(ctx, quoteRev(change.Bookmark))
	if err != nil {
		return err
	}
	if head != change.Tip || !r.keys.Owned(r.codename, change.Bookmark) {
		return refusal("jj-vine", "changed-line", "bookmark changed after preflight: "+change.Bookmark, "rerun rjc submit from a fresh snapshot")
	}
	base := change.Base
	if remoteBase && !strings.HasSuffix(base, "@origin") {
		base += "@origin"
		local, err := r.parentRevision(ctx, change.Base)
		if err != nil {
			return err
		}
		remote, err := r.one(ctx, quoteRev(base))
		if err != nil {
			return err
		}
		if local != remote {
			return refusal("jj-vine", "unpublished-base", "parent boundary is not published: "+change.Base, "submit the lower bookmark first")
		}
	}
	return r.checkEngineSelectionAt(ctx, change, base, "bookmarks(exact:"+quoteRev(change.Bookmark)+")")
}

func (r *repository) checkEngineSelectionAt(ctx context.Context, change Change, base, explicit string) error {
	selection := "(" + explicit + " | (::" + explicit + " & mine() & bookmarks())) ~ ::(trunk() | " + quoteRev(base) + ")"
	bookmarks, err := r.localBookmarks(ctx, selection)
	if err != nil {
		return err
	}
	if len(bookmarks) != 1 || bookmarks[0] != change.Bookmark {
		return refusal("jj-vine", "expanded-line", "engine selection is not the single validated bookmark: "+strings.Join(bookmarks, ", "), "separate foreign or aliased bookmarks before submitting")
	}
	return nil
}
