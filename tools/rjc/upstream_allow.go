package main

import (
	"context"
	"strings"
)

// recordedUpstreamMerge returns a record only when its change ID still matches the bookmark tip.
func (r *repository) recordedUpstreamMerge(ctx context.Context, bookmark, tip string) (UpstreamRecord, bool, error) {
	if r.profile.Posture != forkTracking {
		return UpstreamRecord{}, false, nil
	}
	rec, exists, err := (fileBaseline{root: r.stateDir, keys: r.keys}).LoadUpstream(bookmark)
	if err != nil || !exists {
		return rec, false, err
	}
	if rec.Repo != r.profile.Name || rec.Change == "" {
		return rec, false, nil
	}
	change, err := r.jj(ctx, "log", "--no-graph", "-r", quoteRev(tip), "-T", "change_id")
	if err != nil {
		return rec, false, err
	}
	return rec, change == rec.Change, nil
}

// allowedMerge checks the complete recorded merge shape before any mutation.
func (r *repository) allowedMerge(ctx context.Context, line []Change, merges []string) (bool, error) {
	if r.profile.Posture != forkTracking || len(line) != 1 || len(merges) != 1 || !line[0].Owned || merges[0] != line[0].Tip {
		return false, nil
	}
	rec, exists, err := (fileBaseline{root: r.stateDir, keys: r.keys}).LoadUpstream(line[0].Bookmark)
	if err != nil || !exists {
		return false, err
	}
	return r.allowedMergeWithRecord(ctx, line, merges, rec)
}

func (r *repository) allowedMergeWithRecord(ctx context.Context, line []Change, merges []string, rec UpstreamRecord) (bool, error) {
	if r.profile.Posture != forkTracking || len(line) != 1 || len(merges) != 1 || !line[0].Owned || merges[0] != line[0].Tip {
		return false, nil
	}
	if rec.Repo != r.profile.Name || !fullCommitIDRE.MatchString(rec.Release) || !fullCommitIDRE.MatchString(rec.Upstream) || rec.Change == "" {
		return false, nil
	}
	change, err := r.jj(ctx, "log", "--no-graph", "-r", quoteRev(line[0].Tip), "-T", "change_id")
	if err != nil {
		return false, err
	}
	if change != rec.Change {
		return false, nil
	}
	bookmarks, err := r.localBookmarks(ctx, quoteRev(line[0].Tip))
	if err != nil {
		return false, err
	}
	if len(bookmarks) != 1 || bookmarks[0] != line[0].Bookmark {
		return false, nil
	}
	parents, err := r.jj(ctx, "log", "--no-graph", "-r", quoteRev(line[0].Tip), "-T", `parents.map(|p| p.commit_id()).join("\n")`)
	if err != nil {
		return false, err
	}
	ids := strings.Split(parents, "\n")
	if len(ids) != 2 || ids[1] != rec.Upstream {
		return false, nil
	}
	if r.profile.Mirror == "" {
		return false, nil
	}
	trunk := quoteRev(r.profile.TrunkRemote())
	mirror := quoteRev(r.profile.Mirror + "@origin")
	for _, rev := range []string{quoteRev(ids[0]) + " & ::" + trunk, quoteRev(ids[1]) + " & ::" + mirror, quoteRev(ids[1]) + " ~ ::" + trunk} {
		found, err := r.revisions(ctx, rev)
		if err != nil {
			return false, err
		}
		if len(found) != 1 {
			return false, nil
		}
	}
	owned, err := r.revisions(ctx, "("+quoteRev(ids[0])+".."+quoteRev(line[0].Tip)+") ~ ::"+mirror)
	if err != nil {
		return false, err
	}
	if len(owned) != 1 || owned[0] != line[0].Tip {
		return false, nil
	}
	conflicts, err := r.revisions(ctx, "conflicts() & "+quoteRev(line[0].Tip))
	if err != nil {
		return false, err
	}
	return len(conflicts) == 0, nil
}
