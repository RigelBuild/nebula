// SPDX-License-Identifier: MIT OR Apache-2.0

package prgates

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	MinTitleRunes      = 20
	MaxTitleRunes      = 90
	metadataBoundsGate = "metadata-bounds"
)

var htmlCommentRE = regexp.MustCompile(`(?s)<!--.*?-->`)
var stackBlockRE = regexp.MustCompile(`(?s)<!-- start jj-vine stack -->.*?<!-- end jj-vine stack -->`)
var headingRE = regexp.MustCompile(`^#{1,6}(?:\s|$)`)

func MetadataBounds(facts BoundsFacts) Result {
	if facts.AuthorIsBot || facts.AuthorLogin == "rigelbuild-ci-bot" || strings.HasPrefix(facts.HeadBranch, "renovate/") || strings.HasPrefix(facts.HeadBranch, "trunk-merge/") {
		return Result{Gate: metadataBoundsGate, Pass: true, Reason: "automation", Message: "Generated automation metadata is exempt from authored bounds."}
	}
	if !issuePrefixRE.MatchString(facts.IssuePrefix) {
		return invalidPrefix(metadataBoundsGate, facts.IssuePrefix)
	}
	footerRE := regexp.MustCompile(`(?i)^(?:spec-impact:|(?:refs|closes|fixes)\s+` + facts.IssuePrefix + `-[0-9]+\b|co-authored-by:)`)
	title := strings.TrimSpace(facts.Title)
	length := utf8.RuneCountInString(title)
	if strings.ContainsAny(title, "\r\n") || length < MinTitleRunes || length > MaxTitleRunes {
		return Result{Gate: metadataBoundsGate, Reason: "title", Message: fmt.Sprintf("Trimmed title has %d Unicode code points; require 20 to 90 on one line.", length)}
	}
	body := htmlCommentRE.ReplaceAllString(stackBlockRE.ReplaceAllString(facts.Body, ""), "")
	words := 0
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		if headingRE.MatchString(line) || footerRE.MatchString(line) {
			continue
		}
		words += len(strings.Fields(line))
	}
	if words < 40 || words > 300 {
		return Result{Gate: metadataBoundsGate, Reason: "body", Message: fmt.Sprintf("Authored body has %d whitespace-delimited words; require 40 to 300, excluding comments, headings and footers.", words)}
	}
	return Result{Gate: metadataBoundsGate, Pass: true, Reason: "within-bounds", Message: fmt.Sprintf("Title has %d code points; authored body has %d words.", length, words)}
}
