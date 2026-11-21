// SPDX-License-Identifier: MIT OR Apache-2.0

package issueowner

import (
	"regexp"
	"strings"
)

// NonAgentBranchPrefixes names branch prefixes that automation owns, not an agent.
var NonAgentBranchPrefixes = []string{"renovate", "dependabot", "trunk-merge", "graphite-base"}

var (
	branchCodenamePattern    = regexp.MustCompile(`(?i)^[a-z][a-z0-9-]*$`)
	branchDescriptionPattern = regexp.MustCompile(`(?i)^[a-z0-9][a-z0-9-]*$`)
)

// BranchReferenceResolver supplies the issue-reference rules used by a branch parser.
type BranchReferenceResolver interface {
	IssueRefPattern() *regexp.Regexp
	CanonicalRef(ref string) string
}

// BranchClaim is an issue and owner codename parsed from an agent branch.
type BranchClaim struct {
	Identifier string
	Codename   string
	Branch     string
	SlashForm  bool
}

// ParseBranchClaim parses a slash-form or legacy hyphen-form agent branch name.
// isKnownCodename resolves ambiguous legacy prefixes against the live roster;
// slashCodenames prevents a shorter legacy prefix from overriding a slash claim.
func ParseBranchClaim(branch string, refs BranchReferenceResolver, isKnownCodename func(string) bool, slashCodenames map[string]struct{}) (BranchClaim, bool) {
	branch = strings.TrimSpace(branch)
	if claim, ok := parseSlashBranch(branch, refs); ok {
		claim.SlashForm = true
		return claim, true
	}
	return parseLegacyBranch(branch, refs, isKnownCodename, slashCodenames)
}

// ParseBranch returns the canonical issue identifier from an agent branch.
func ParseBranch(branch string, refs BranchReferenceResolver) (string, bool) {
	claim, ok := ParseBranchClaim(branch, refs, nil, nil)
	if !ok || IsReservedCodename(claim.Codename) {
		return "", false
	}
	return claim.Identifier, true
}

// BranchContainsIssueRef reports whether branch text contains a reference recognized by refs.
func BranchContainsIssueRef(branch string, refs BranchReferenceResolver) bool {
	return branchIssueRefPattern(refs).FindStringIndex(branch) != nil
}

// IsAutomationBranch reports whether a branch has a prefix reserved for automation.
func IsAutomationBranch(branch string) bool {
	for _, prefix := range NonAgentBranchPrefixes {
		if strings.HasPrefix(branch, prefix) {
			return true
		}
	}
	return false
}

// ResolveBranchCodename resolves the longest known codename prefix, or the full prefix if none match.
func ResolveBranchCodename(prefix string, isKnownCodename func(string) bool) string {
	full := strings.ToLower(prefix)
	if isKnownCodename == nil {
		return full
	}
	segments := strings.Split(full, "-")
	for take := len(segments); take > 0; take-- {
		candidate := strings.Join(segments[:take], "-")
		if isKnownCodename(candidate) {
			return candidate
		}
	}
	return full
}

func parseSlashBranch(branch string, refs BranchReferenceResolver) (BranchClaim, bool) {
	if IsAutomationBranch(branch) {
		return BranchClaim{}, false
	}
	slashAt := strings.IndexByte(branch, '/')
	if slashAt <= 0 || strings.LastIndexByte(branch, '/') != slashAt {
		return BranchClaim{}, false
	}
	codename := strings.ToLower(branch[:slashAt])
	if !branchCodenamePattern.MatchString(codename) {
		return BranchClaim{}, false
	}
	refAndDescription := branch[slashAt+1:]
	match := branchIssueRefPattern(refs).FindStringIndex(refAndDescription)
	if match == nil || match[0] != 0 || match[1] >= len(refAndDescription) || refAndDescription[match[1]] != '-' {
		return BranchClaim{}, false
	}
	description := refAndDescription[match[1]+1:]
	if !branchDescriptionPattern.MatchString(description) {
		return BranchClaim{}, false
	}
	return BranchClaim{
		Identifier: refs.CanonicalRef(refAndDescription[:match[1]]),
		Codename:   codename,
		Branch:     branch,
		SlashForm:  true,
	}, true
}

func parseLegacyBranch(branch string, refs BranchReferenceResolver, isKnownCodename func(string) bool, slashCodenames map[string]struct{}) (BranchClaim, bool) {
	if branch == "" || IsAutomationBranch(branch) {
		return BranchClaim{}, false
	}
	for refStart := 2; refStart < len(branch); refStart++ {
		if branch[refStart-1] != '-' || !branchCodenamePattern.MatchString(branch[:refStart-1]) {
			continue
		}
		match := branchIssueRefPattern(refs).FindStringIndex(branch[refStart:])
		if match == nil || match[0] != 0 {
			continue
		}
		refEnd := refStart + match[1]
		if refEnd < len(branch) && branch[refEnd] != '-' {
			continue
		}
		prefix := strings.ToLower(branch[:refStart-1])
		codename := prefix
		if _, isSlash := slashCodenames[prefix]; !isSlash {
			codename = ResolveBranchCodename(prefix, isKnownCodename)
		}
		return BranchClaim{Identifier: refs.CanonicalRef(branch[refStart:refEnd]), Codename: codename, Branch: branch}, true
	}
	return BranchClaim{}, false
}

func branchIssueRefPattern(refs BranchReferenceResolver) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(?:` + refs.IssueRefPattern().String() + `)`)
}
