package main

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/RigelBuild/nebula/tools/go-lib/prgates"
	"github.com/samber/oops"
)

const (
	metadataBoundsGate = "metadata-bounds"
	specImpactGate     = "spec-impact"
)

type ForkPosture string

const (
	forkNone     ForkPosture = "none"
	forkTracking ForkPosture = "tracking"
)

// Profile is one repository's rjc policy. ProfileFor fills the deployment
// fields from the caller's Config, so no value comes from nebula itself.
type Profile struct {
	Name          string      `json:"name"`
	IssuePrefix   string      `json:"issuePrefix"`
	SpecDirs      []string    `json:"specDirs"`
	CIGates       []string    `json:"ciGates"`
	LocalGates    []string    `json:"-"`
	CoAuthor      string      `json:"-"`
	Reviewer      string      `json:"-"`
	GateRunnerDir string      `json:"-"`
	RootChecksDir string      `json:"-"`
	DevenvNixpkgs string      `json:"-"`
	PR            bool        `json:"pr"`
	Posture       ForkPosture `json:"posture"`
	Trunk         string      `json:"trunk"`
	Mirror        string      `json:"mirror"`
	RemoteHooks   bool        `json:"remoteHooks"`
	Hooks         bool        `json:"hooks"`
	NoHooksReason string      `json:"noHooksReason"`
	LocalFastTier bool        `json:"localFastTier"`
	HookReports   bool        `json:"hookReports"`
	DesignLabel   bool        `json:"designLabel"`
	BaseFilesets  []string    `json:"baseFilesets"`
	SkipChecks    []string    `json:"skipChecks"`
}

func (p Profile) TrunkRemote() string { return p.Trunk + "@origin" }

// ProfileFor returns the profile for originURL. An unlisted repository uses
// the caller's default profile when present; otherwise it is refused.
func ProfileFor(cfg Config, originURL string) (Profile, error) {
	origin := strings.TrimSpace(originURL)
	var repo string
	if rest, ok := strings.CutPrefix(origin, "git@github.com:"); ok {
		repo = rest
	} else {
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Hostname() != "github.com" {
			return Profile{}, oops.Errorf("unsupported origin; use a supported GitHub repository")
		}
		repo = strings.TrimPrefix(parsed.Path, "/")
	}
	repo = strings.TrimSuffix(strings.TrimRight(repo, "/"), ".git")
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || !slices.ContainsFunc(cfg.Owners, func(o string) bool { return strings.EqualFold(o, owner) }) {
		return Profile{}, oops.Errorf("unsupported repository owner")
	}
	if name == "" || name == "." || name == ".." || !repoNameRE.MatchString(name) {
		return Profile{}, oops.Errorf("unsupported repository name")
	}
	for from, to := range cfg.Aliases {
		if strings.EqualFold(repo, from) {
			repo = to
			break
		}
	}
	var p Profile
	found := false
	for key, entry := range cfg.Repos {
		if strings.EqualFold(key, repo) {
			p, found = entry, true
			break
		}
	}
	if !found {
		if cfg.Default == nil {
			return Profile{}, oops.Errorf("repository %s is not listed in the profile file", repo)
		}
		p = *cfg.Default
		p.Name = repo
	}
	p.LocalGates = []string{"bookmark-name", "issue-id"}
	p.CoAuthor = cfg.CoAuthor
	for configuredOwner, coAuthor := range cfg.CoAuthorByOwner {
		if strings.EqualFold(configuredOwner, owner) {
			p.CoAuthor = coAuthor
			break
		}
	}
	p.Reviewer = cfg.Reviewer
	p.GateRunnerDir = cfg.GateRunnerDir
	p.RootChecksDir = cfg.RootChecksDir
	p.DevenvNixpkgs = cfg.DevenvNixpkgs
	return p, nil
}

var repoNameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
var issuePrefixRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)
var conventionalTypes = []string{
	"build", "chore", "ci", "docs", "feat", "fix", "perf", "refactor", "revert", "style", "test",
}

const typeShape = `[a-z]+(?:\([^\r\n()]+\))?!?:`

var titleTypePrefixRE = regexp.MustCompile(`^(?:(?i:(?:` + strings.Join(conventionalTypes, "|") + `))(?:\([^\r\n()]+\))?!?:|` + typeShape + `\s+)`)
var titleWithTypeRE = regexp.MustCompile(`^` + typeShape + `\s+(.+)$`)
var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

// issueKeys holds the issue-key rules for one issue prefix. newIssueKeys
// compiles every regex once; callers keep the value on the repository and
// never compile per call.
type issueKeys struct {
	prefix          string
	issueSuffixRE   *regexp.Regexp
	trailingRE      *regexp.Regexp
	titleIssueRE    *regexp.Regexp
	issueRE         *regexp.Regexp
	bookmarkIssueRE *regexp.Regexp
	issueReasonRE   *regexp.Regexp
	p0SkipRE        *regexp.Regexp
}

// newIssueKeys builds the rules for prefix, such as "EX" for keys like EX-123.
func newIssueKeys(prefix string) (issueKeys, error) {
	if !issuePrefixRE.MatchString(prefix) {
		return issueKeys{}, oops.Errorf("issue prefix %q is not a letter followed by letters or digits", prefix)
	}
	upper := strings.ToUpper(prefix)
	lower := strings.ToLower(prefix)
	return issueKeys{
		prefix:          upper,
		issueSuffixRE:   regexp.MustCompile(`(?i) \(` + upper + `-[0-9]+(?:, ` + upper + `-[0-9]+)*\)$`),
		titleIssueRE:    regexp.MustCompile(`(?i)\b` + upper + `-[0-9]+`),
		issueRE:         regexp.MustCompile(`^` + upper + `-([0-9]+)$`),
		bookmarkIssueRE: regexp.MustCompile(`/` + lower + `-([0-9]+)-`),
		issueReasonRE:   regexp.MustCompile(`^` + upper + `-[1-9][0-9]*$`),
		p0SkipRE:        regexp.MustCompile(`^p0:` + upper + `-[1-9][0-9]*$`),
		trailingRE:      regexp.MustCompile(`(?im)^(?:(?:Refs|Closes|Fixes)\s+` + upper + `-[0-9]+.*|Co-authored-by:.*)\r?\n?`),
	}, nil
}

func (k issueKeys) validateAuthoredTitle(title string) error {
	if k.titleIssueRE.MatchString(title) {
		return refusal("metadata", "title", "authored title contains an issue reference", "remove "+k.prefix+"-<n> from --title; rjc adds issue references")
	}
	return nil
}

func (k issueKeys) FullTitle(title string, issues []int) (string, error) {
	title = k.issueSuffixRE.ReplaceAllString(strings.TrimSpace(title), "")
	if err := k.validateAuthoredTitle(title); err != nil {
		return "", err
	}
	if len(issues) == 0 || strings.ContainsAny(title, "\r\n") || !titleWithTypeRE.MatchString(title) {
		return "", refusal("metadata", "title", "require a positive issue and a one-line Conventional Commits title", `rjc new --issue `+k.prefix+`-<n> --title "feat(scope): subject"`)
	}
	refs := make([]string, 0, len(issues))
	for _, issue := range issues {
		if issue <= 0 {
			return "", refusal("metadata", "title", "require positive issues", "use --issue "+k.prefix+"-<n>")
		}
		refs = append(refs, fmt.Sprintf("%s-%d", k.prefix, issue))
	}
	title += " (" + strings.Join(refs, ", ") + ")"
	length := utf8.RuneCountInString(title)
	if length < prgates.MinTitleRunes || length > prgates.MaxTitleRunes {
		return "", refusal("metadata", metadataBoundsGate, fmt.Sprintf("trimmed title has %d Unicode code points; require %d to %d on one line", length, prgates.MinTitleRunes, prgates.MaxTitleRunes), "edit --title without truncating its issue reference")
	}
	return title, nil
}

func (k issueKeys) BookmarkName(codename string, issue int, title string) (string, error) {
	full, err := k.FullTitle(title, []int{issue})
	if err != nil {
		return "", err
	}
	subject := titleWithTypeRE.FindStringSubmatch(k.issueSuffixRE.ReplaceAllString(full, ""))[1]
	slug := strings.Trim(slugRE.ReplaceAllString(strings.ToLower(subject), "-"), "-")
	if len(slug) > 40 {
		cut := strings.LastIndexByte(slug[:41], '-')
		if cut >= 0 {
			slug = slug[:cut]
		} else {
			return "", refusal("new", "bookmark-name", "title subject has no whole word within 40 characters", "write a subject with a whole word within the first 40 characters")
		}
	}
	bookmark := fmt.Sprintf("%s/%s-%d-%s", codename, strings.ToLower(k.prefix), issue, slug)
	if result := k.branchName(bookmark); !result.Pass {
		return "", refusal("new", "bookmark-name", result.Message, "use a non-reserved --codename and a subject containing ASCII letters or digits")
	}
	return bookmark, nil
}

func (k issueKeys) branchName(bookmark string) prgates.Result {
	return prgates.BranchName(prgates.BranchFacts{HeadRef: bookmark, IssuePrefix: k.prefix})
}

// Owned reports whether bookmark belongs to codename: its prefix is codename/
// and the branch name is a conforming agent branch.
func (k issueKeys) Owned(codename, bookmark string) bool {
	return codename != "" && strings.HasPrefix(bookmark, codename+"/") && k.branchName(bookmark).Pass
}

func (k issueKeys) issueNumber(value string) (int, error) {
	match := k.issueRE.FindStringSubmatch(value)
	if len(match) != 2 {
		return 0, refusal("metadata", "issue-id", "issue must be "+k.prefix+"-<positive number>", "use --issue "+k.prefix+"-<n>")
	}
	number, err := strconv.Atoi(match[1])
	if err != nil || number <= 0 {
		return 0, refusal("metadata", "issue-id", "issue number is out of range", "use --issue "+k.prefix+"-<positive number>")
	}
	return number, nil
}

func (k issueKeys) bookmarkIssue(bookmark string) (int, error) {
	match := k.bookmarkIssueRE.FindStringSubmatch(bookmark)
	if len(match) != 2 {
		return 0, refusal("metadata", "issue-id", "bookmark has no issue number", `rjc new --issue `+k.prefix+`-<n> --title "feat(scope): subject"`)
	}
	return k.issueNumber(k.prefix + "-" + match[1])
}
