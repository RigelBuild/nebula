package issueowner

import (
	"regexp"
	"strings"
	"testing"
)

type branchParserFixture struct {
	pattern *regexp.Regexp
}

func (b branchParserFixture) IssueRefPattern() *regexp.Regexp { return b.pattern }
func (b branchParserFixture) CanonicalRef(ref string) string {
	upper := strings.ToUpper(ref)
	if strings.HasPrefix(upper, "SEA-") {
		return "RIG-" + strings.TrimPrefix(upper, "SEA-")
	}
	return upper
}

func TestParseBranchClaim(t *testing.T) {
	refs := branchParserFixture{pattern: regexp.MustCompile(`(?i)\b(?:SEA|RIG)-\d+\b`)}
	tests := []struct {
		name         string
		branch       string
		known        func(string) bool
		slash        map[string]struct{}
		want         string
		wantCodename string
		wantSlash    bool
	}{
		{name: "slash form canonicalizes issue and codename", branch: " CI-Build/sea-1143-x ", want: "RIG-1143", wantCodename: "ci-build", wantSlash: true},
		{name: "legacy form resolves longest known prefix", branch: "ci-build-sea-1143-x", known: func(c string) bool { return c == "ci" || c == "ci-build" }, want: "RIG-1143", wantCodename: "ci-build"},
		{name: "slash codename wins against shorter legacy prefix", branch: "ci-build-sea-1143-x", known: func(c string) bool { return c == "ci" }, slash: map[string]struct{}{"ci-build": {}}, want: "RIG-1143", wantCodename: "ci-build"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			claim, ok := ParseBranchClaim(tc.branch, refs, tc.known, tc.slash)
			if !ok || claim.Identifier != tc.want || claim.Codename != tc.wantCodename || claim.SlashForm != tc.wantSlash {
				t.Fatalf("claim = %+v, %t", claim, ok)
			}
		})
	}
	for _, branch := range []string{"release/notes-rig-1-update", "ci-build/rig-1143", "renovate/rig-1143-update", "mintaka/rig-3-x"} {
		if claim, ok := ParseBranchClaim(branch, refs, nil, nil); ok && !IsReservedCodename(claim.Codename) {
			t.Errorf("ParseBranchClaim(%q) = %+v, want no agent claim", branch, claim)
		}
	}
}
