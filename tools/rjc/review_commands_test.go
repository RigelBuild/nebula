package main

import (
	"regexp"
	"strings"
	"testing"
)

//nolint:tparallel // Subtests share the parent fixture.
func TestCheckReviewHead(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): test review head", "")
	f.commit(t, "review.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", bookmark)
	head := jjTest(t, f.repo, "log", "--no-graph", "-r", bookmark, "-T", "commit_id")
	change := Change{Bookmark: bookmark, Tip: head, Base: mainOrigin}
	meta := Metadata{HeadRef: bookmark, HeadSHA: head, BaseRef: "main"}
	tests := []struct {
		name, reviewed string
		meta           Metadata
		change         Change
		gate           string
	}{
		{name: "current", reviewed: head, meta: meta, change: change},
		{name: "old review", reviewed: strings.Repeat("a", 40), meta: meta, change: change, gate: "head-mismatch"},
		{name: "stale tip", reviewed: head, meta: meta, change: Change{Tip: strings.Repeat("a", 40), Base: mainOrigin}, gate: "head-mismatch"},
		{name: "moved base", reviewed: head, meta: Metadata{HeadRef: bookmark, HeadSHA: head, BaseRef: "other"}, change: change, gate: "base-mismatch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := f.repo.checkReviewHead(f.ctx, bookmark, tt.meta, tt.change, tt.reviewed)
			if tt.gate == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				assertRefusalGate(t, err, tt.gate)
			}
		})
	}
}

func TestAllClearCommentMatchesMergeBot(t *testing.T) {
	t.Parallel()
	const reviewed = "0123456789abcdef0123456789abcdef01234567"
	// The merge bot's all-clear pattern.
	mergeAllClear := regexp.MustCompile("(?i)^all[- ]clear (?:at|for) `?([0-9a-f]{8,40})`?\\.?$")
	// Same parts, in the same order, as the body that noteCommand posts.
	body := allClearHeader(reviewed, "main") + FormatMarker(Marker{Kind: markerAllClear, Reviewed: reviewed, Base: "main"})
	first := ""
	for line := range strings.SplitSeq(body, "\n") {
		if strings.TrimSpace(line) != "" {
			first = strings.TrimSpace(line)
			break
		}
	}
	if first != "All-clear at "+reviewed {
		t.Fatalf("first non-blank line = %q", first)
	}
	m := mergeAllClear.FindStringSubmatch(first)
	if m == nil || m[1] != reviewed {
		t.Fatalf("merge-bot pattern match = %q, want full reviewed sha %s", m, reviewed)
	}
	markers := ParseMarkers(body, "example-agent", "https://example.test/comment")
	if len(markers) != 1 || markers[0].Kind != markerAllClear || markers[0].Reviewed != reviewed || markers[0].Base != "main" {
		t.Fatalf("markers = %#v, want one all-clear for %s on main", markers, reviewed)
	}
}
