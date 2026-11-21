package main

import (
	"context"
	"fmt"
	"html"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// validHoldReason accepts one printable line of up to skipReasonMax runes.
func validHoldReason(reason string) bool {
	if reason == "" || utf8.RuneCountInString(reason) > skipReasonMax {
		return false
	}
	return strings.IndexFunc(reason, func(r rune) bool { return !unicode.IsPrint(r) }) < 0
}

// holdThenReady posts the Hold on pr, then promotes a PR this run opened as draft only for the Hold.
// A failed post leaves that PR draft, so the merge bot refuses it.
func (r *repository) holdThenReady(ctx context.Context, f *forgeClient, pr int, reason string, promote bool) error {
	if err := postHold(ctx, f, r.profile.Name, pr, reason); err != nil {
		return holdFailure(pr, promote, err)
	}
	if !promote {
		return nil
	}
	if err := f.markReady(ctx, r.profile.Name, pr); err != nil {
		return &failure{
			stage:  submitCommandName,
			gate:   toolFailureGate,
			reason: fmt.Sprintf("PR %d has its Hold but is still draft: %v", pr, err),
			fix:    fmt.Sprintf("run gh pr ready %d -R %s", pr, r.profile.Name),
			code:   3,
			cause:  err,
		}
	}
	return nil
}

// postHold posts `Hold: <reason>` on pr unless the viewer's latest Hold already has that reason and is still in force.
// The merge bot (comment_clears_hold) lifts a login's Hold only at that login's all-clear or explicit lift line,
// so other later comments leave it in force.
func postHold(ctx context.Context, f *forgeClient, repo string, pr int, reason string) error {
	viewer, err := f.viewer(ctx)
	if err != nil {
		return err
	}
	comments, err := f.issueComments(ctx, repo, pr)
	if err != nil {
		return err
	}
	if !holdInForce(comments, viewer, reason) {
		_, err = f.postComment(ctx, repo, pr, "Hold: "+reason)
	}
	return err
}

// holdInForce reports whether the viewer's latest Hold line has reason and no later viewer comment clears it.
// It parses only plain comments (see plainComment): a non-plain viewer comment from the Hold on means repost,
// because the merge bot's Unicode-aware parsing can read it differently, and a duplicate Hold is harmless.
func holdInForce(comments []issueComment, viewer, reason string) bool {
	want := normalizeActive(reason)
	for _, c := range slices.Backward(comments) {
		if c.User.Login != viewer {
			continue
		}
		if !plainComment(c.Body) {
			return false
		}
		if latest, ok := lastHoldReason(c.Body); ok {
			return latest == want
		}
		if clearsHold(c) {
			return false
		}
	}
	return false
}

// plainComment reports whether body is printable ASCII, `\n` and `\t` only, with no `&` that could start an HTML entity.
// For such text Go's ASCII regexp and line splitting agree with the merge bot's Python.
func plainComment(body string) bool {
	for i := range len(body) {
		b := body[i]
		if b == '&' || (b != '\n' && b != '\t' && (b < 0x20 || b > 0x7e)) {
			return false
		}
	}
	return true
}

var (
	// holdLinePattern is comment_has_hold's `^\s*Hold:` with the rest of the line captured as the reason.
	holdLinePattern = regexp.MustCompile(`(?i)^\s*Hold:(.*)$`)
	// liftPattern is the merge bot's LEADING_LIFT_RE.
	liftPattern = regexp.MustCompile(`(?i)^\s*(?:unhold|hold[\s-]+(?:lifted|cleared))\b`)
	// allClearPattern is the merge bot's POSITIVE_ALL_CLEAR_RE, used with review_verdict's leading `#+\s*` strip.
	allClearPattern = regexp.MustCompile("(?i)^all[- ]clear (?:at|for) `?[0-9a-f]{8,40}`?\\.?$")
	headingPattern  = regexp.MustCompile(`^#+\s*`)
	// inlineCodePattern and listMarkerPattern are comment_lifts' inline-code strip and leading-marker strip.
	inlineCodePattern = regexp.MustCompile("`[^`\n]*`")
	listMarkerPattern = regexp.MustCompile(`^(?:#+|[-+*]|\d+[.)])\s+`)
	fencePattern      = regexp.MustCompile("^\\s*(`{3,}|~{3,})")
	strikePattern     = regexp.MustCompile(`~~.*?~~`)
)

// normalizeActive applies active_text's final pass: drop `*`, map `_` to `-`, and drop Unicode format (Cf) characters.
func normalizeActive(text string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '*', unicode.Is(unicode.Cf, r):
			return -1
		case r == '_':
			return '-'
		}
		return r
	}, text)
}

// activeText ports the merge bot's active_text: unescape HTML, drop fenced and quoted lines,
// remove strikethrough, then normalizeActive. Lines are joined with "\n".
func activeText(body string) string {
	var lines []string
	fence := ""
	for line := range strings.Lines(html.UnescapeString(body)) {
		line = strings.TrimRight(line, "\r\n")
		if m := fencePattern.FindStringSubmatch(line); m != nil {
			switch {
			case fence == "":
				fence = m[1]
			case m[1][0] == fence[0] && len(m[1]) >= len(fence):
				fence = ""
			}
			continue
		}
		if fence == "" && !strings.HasPrefix(strings.TrimLeft(line, " \t"), ">") {
			lines = append(lines, strikePattern.ReplaceAllString(line, ""))
		}
	}
	return normalizeActive(strings.Join(lines, "\n"))
}

// lastHoldReason returns the trimmed reason of body's last active Hold line.
func lastHoldReason(body string) (string, bool) {
	reason, found := "", false
	for line := range strings.SplitSeq(activeText(body), "\n") {
		if m := holdLinePattern.FindStringSubmatch(line); m != nil {
			reason, found = strings.TrimSpace(m[1]), true
		}
	}
	return reason, found
}

// clearsHold reports whether c is an all-clear or a lift, following comment_clears_hold.
// Any reviewed head counts, and comment_lifts' conditional-tail refusals are skipped: both err toward a repost.
// Also safe: review_verdict's NEGATIVE_VERDICT_RE veto is skipped, a marker-only all-clear clears, and `\b` is ASCII.
func clearsHold(c issueComment) bool {
	for _, m := range ParseMarkers(c.Body, c.User.Login, c.HTMLURL) {
		if m.Kind == markerAllClear {
			return true
		}
	}
	text := activeText(c.Body)
	// review_verdict: the first non-blank active line, leading `#` stripped, is the verdict.
	for line := range strings.Lines(text) {
		if first := strings.TrimSpace(line); first != "" {
			if allClearPattern.MatchString(headingPattern.ReplaceAllString(first, "")) {
				return true
			}
			break
		}
	}
	// comment_lifts: drop inline code, trim, drop one leading list or heading marker, then match the lift.
	lift := strings.TrimSpace(inlineCodePattern.ReplaceAllString(text, ""))
	lift = listMarkerPattern.ReplaceAllString(lift, "")
	return liftPattern.MatchString(lift)
}

// holdFailure names the PR whose Hold did not post, so the caller never reports success without it.
// leftDraft is true when this run opened the PR as draft for the Hold and kept it draft.
func holdFailure(pr int, leftDraft bool, cause error) error {
	reason := fmt.Sprintf("PR %d was pushed, but the Hold comment was not posted: %v", pr, cause)
	fix := "rerun rjc submit --hold \"<reason>\"; until a Hold posts, the merge bot can merge the PR"
	if leftDraft {
		reason = fmt.Sprintf("PR %d was opened as draft and left draft, because the Hold comment was not posted: %v", pr, cause)
		fix = fmt.Sprintf("rerun rjc submit --hold \"<reason>\", then mark PR %d ready", pr)
	}
	return &failure{stage: submitCommandName, gate: toolFailureGate, reason: reason, fix: fix, code: 3, cause: cause}
}
