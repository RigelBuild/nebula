// SPDX-License-Identifier: MIT OR Apache-2.0

// Package prgates holds the pull request decisions shared by CI and rjc.
package prgates

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/RigelBuild/nebula/tools/go-lib/issueowner"
)

type Result struct {
	Gate, Reason, Message string
	Pass                  bool
}

// IssuePrefix is the issue key prefix the caller tracks work under, such as
// "EX" for keys like EX-123. Each gate that reads issue keys requires it.
type BranchFacts struct {
	HeadRef, AuthorLogin string
	AuthorIsBot          bool
	IssuePrefix          string
}
type TitleFacts struct {
	Title       string
	AuthorIsBot bool
	IssuePrefix string
}

// SpecImpactFacts.SpecDirs lists the repository directories, each ending in
// "/", that hold living specs.
type SpecImpactFacts struct {
	ChangedFiles     []string
	Body, HeadBranch string
	SpecDirs         []string
}
type BoundsFacts struct {
	Title, Body, AuthorLogin, HeadBranch string
	AuthorIsBot                          bool
	IssuePrefix                          string
}

const (
	branchNameGate = "branch-name"
	titleIssueGate = "pr-title-issue-ref"
	specImpactGate = "spec-impact"
)

var issuePrefixRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)
var specImpactRE = regexp.MustCompile(`(?im)^\s*>?\s*spec-impact:\s*(\S.*)$`)

// invalidPrefix reports a gate whose caller supplied no usable issue prefix.
func invalidPrefix(gate, prefix string) Result {
	return Result{Gate: gate, Reason: "issue-prefix", Message: fmt.Sprintf("Issue key prefix %q is not a letter followed by letters or digits.", prefix)}
}

func BranchName(pr BranchFacts) Result {
	if pr.AuthorIsBot || pr.AuthorLogin == "rigelbuild-ci-bot" {
		return Result{Gate: branchNameGate, Pass: true, Reason: "bot"}
	}
	if !issuePrefixRE.MatchString(pr.IssuePrefix) {
		return invalidPrefix(branchNameGate, pr.IssuePrefix)
	}
	lower := strings.ToLower(pr.IssuePrefix)
	branchRE := regexp.MustCompile(`^[a-z][a-z0-9-]*/` + lower + `-[0-9]+-[a-z0-9][a-z0-9-]*$`)
	codename, _, _ := strings.Cut(pr.HeadRef, "/")
	if branchRE.MatchString(pr.HeadRef) && !slices.Contains(issueowner.ReservedCodenames, codename) {
		return Result{Gate: branchNameGate, Pass: true, Reason: "conforms"}
	}
	return Result{Gate: branchNameGate, Message: fmt.Sprintf("Branch '%s' does not match <codename>/%s-<n>-<kebab-desc> (lowercase; codename not one of %s). A pushed branch cannot be renamed: reopen the PR from a conforming branch.", pr.HeadRef, lower, strings.Join(issueowner.ReservedCodenames, ", "))}
}

func TitleIssueRef(facts TitleFacts) Result {
	if facts.AuthorIsBot {
		return Result{Gate: titleIssueGate, Pass: true, Reason: "exempt-bot", Message: "PR is bot-authored (dependency automation), exempt from the issue-reference requirement."}
	}
	if !issuePrefixRE.MatchString(facts.IssuePrefix) {
		return invalidPrefix(titleIssueGate, facts.IssuePrefix)
	}
	upper := strings.ToUpper(facts.IssuePrefix)
	issueRefRE := regexp.MustCompile(`(?i)\b` + upper + `-[0-9]+`)
	if ref := issueRefRE.FindString(facts.Title); ref != "" {
		return Result{Gate: titleIssueGate, Pass: true, Reason: "has-ref", Message: fmt.Sprintf("PR title references %s.", strings.ToUpper(ref))}
	}
	return Result{Gate: titleIssueGate, Reason: "missing", Message: fmt.Sprintf("This PR's title carries no issue reference. Every PR must name the issue it addresses as '%[1]s-NNN' somewhere in its title (e.g. 'fix(ci): correct the fan-out base ref (%[1]s-2179)'). Edit the PR title to include the %[1]s-NNN id and re-run the check.", upper)}
}

func SpecImpact(facts SpecImpactFacts) Result {
	for _, prefix := range []string{"renovate/", "trunk-merge/"} {
		if strings.HasPrefix(facts.HeadBranch, prefix) {
			return Result{Gate: specImpactGate, Pass: true, Reason: "exempt-branch", Message: fmt.Sprintf("Branch '%s' is automation-exempt from the spec-impact gate.", facts.HeadBranch)}
		}
	}
	for _, file := range facts.ChangedFiles {
		for _, prefix := range facts.SpecDirs {
			if strings.HasPrefix(file, prefix) {
				return Result{Gate: specImpactGate, Pass: true, Reason: "touches-spec", Message: "PR updates a living spec."}
			}
		}
	}
	if match := specImpactRE.FindStringSubmatch(facts.Body); len(match) > 1 {
		if value := strings.TrimSpace(match[1]); value != "" {
			return Result{Gate: specImpactGate, Pass: true, Reason: "declared", Message: "PR declares Spec-impact: " + value}
		}
	}
	return Result{Gate: specImpactGate, Reason: "missing", Message: "This PR neither updates a living spec (under " + strings.Join(facts.SpecDirs, " or ") + ") nor declares its spec impact. Either update the relevant spec in this PR, or add a 'Spec-impact:' line to the PR description — it must start its own line (a single leading '>' quote or indentation is fine; a mid-sentence mention is not detected). E.g. 'Spec-impact: none' when the change has no externally-visible behavior, or name the spec a follow-up will touch. See the docs and specs guidance in AGENTS.md."}
}
