package issueowner

import (
	"strings"
	"testing"
)

func TestResolveOwnerPrecedence(t *testing.T) {
	tests := []struct {
		name       string
		in         OwnerInput
		wantOwner  string
		wantSource OwnerSource
		wantEv     string
	}{
		{
			name:       "own owner beats parent and branch",
			in:         OwnerInput{Owner: "docs-site", HasOwner: true, Parents: []ParentOwner{{Identifier: "EX-100", Owner: "nix-infra", HasOwner: true}}, BranchCodename: "ci-build"},
			wantOwner:  "docs-site",
			wantSource: SourceOwner,
			wantEv:     "Owner: docs-site",
		},
		{
			name:       "nearest owned parent beats branch",
			in:         OwnerInput{Parents: []ParentOwner{{Identifier: "EX-100", Owner: "nix-infra", HasOwner: true}}, BranchCodename: "ci-build"},
			wantOwner:  "nix-infra",
			wantSource: SourceParent,
			wantEv:     "parent chain EX-100 (owner nix-infra)",
		},
		{
			name: "walks an unowned chain to the first owner",
			in: OwnerInput{Parents: []ParentOwner{
				{Identifier: "EX-1"}, {Identifier: "EX-2"}, {Identifier: "EX-3", Owner: "ci-build", HasOwner: true},
			}},
			wantOwner:  "ci-build",
			wantSource: SourceParent,
			wantEv:     "parent chain EX-1 → EX-2 → EX-3 (owner ci-build)",
		},
		{
			name:       "branch decides when no owner is on the chain",
			in:         OwnerInput{Parents: []ParentOwner{{Identifier: "EX-1"}}, BranchCodename: "livingstone"},
			wantOwner:  "livingstone",
			wantSource: SourceBranch,
			wantEv:     "branch names livingstone",
		},
		{
			name: "no owner, parent, or branch yields an empty verdict",
			in:   OwnerInput{Parents: []ParentOwner{{Identifier: "EX-1"}}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveOwner(tc.in)
			if got.Owner != tc.wantOwner || got.Source != tc.wantSource {
				t.Fatalf("ResolveOwner = {Owner:%q Source:%q}, want {Owner:%q Source:%q}",
					got.Owner, got.Source, tc.wantOwner, tc.wantSource)
			}
			if tc.wantEv != "" && got.Evidence != tc.wantEv {
				t.Fatalf("Evidence = %q, want %q", got.Evidence, tc.wantEv)
			}
		})
	}
}

func TestResolveOwnerConflict(t *testing.T) {
	t.Run("branch codename disagreeing with an Owner line is named", func(t *testing.T) {
		got := ResolveOwner(OwnerInput{Owner: "docs-site", HasOwner: true, BranchCodename: "ci-build"})
		if got.Owner != "docs-site" || got.Source != SourceOwner {
			t.Fatalf("owner precedence broken: %+v", got)
		}
		if !strings.Contains(got.Conflict, "branch codename ci-build disagrees with owner docs-site") {
			t.Fatalf("Conflict = %q, want the named branch/owner disagreement", got.Conflict)
		}
	})
	t.Run("branch codename disagreeing with a parent owner is named", func(t *testing.T) {
		got := ResolveOwner(OwnerInput{Parents: []ParentOwner{{Identifier: "EX-100", Owner: "nix-infra", HasOwner: true}}, BranchCodename: "ci-build"})
		if got.Owner != "nix-infra" || got.Source != SourceParent {
			t.Fatalf("parent precedence broken: %+v", got)
		}
		if !strings.Contains(got.Conflict, "branch codename ci-build disagrees with owner nix-infra") {
			t.Fatalf("Conflict = %q, want the named branch/parent disagreement", got.Conflict)
		}
	})
	t.Run("branch codename equal to the owner is not a conflict", func(t *testing.T) {
		got := ResolveOwner(OwnerInput{Owner: "ci-build", HasOwner: true, BranchCodename: "ci-build"})
		if got.Conflict != "" {
			t.Fatalf("Conflict = %q, want none", got.Conflict)
		}
	})
	t.Run("branch alone is never a conflict", func(t *testing.T) {
		got := ResolveOwner(OwnerInput{BranchCodename: "livingstone"})
		if got.Conflict != "" {
			t.Fatalf("Conflict = %q, want none", got.Conflict)
		}
	})
}

func TestResolveOwnerChainAndTruncated(t *testing.T) {
	tests := []struct {
		name          string
		in            OwnerInput
		wantChain     []string
		wantTruncated bool
		wantOwner     string
	}{
		{
			name: "chain ends at the owning parent",
			in: OwnerInput{Parents: []ParentOwner{
				{Identifier: "EX-1"}, {Identifier: "EX-2", Owner: "ci-build", HasOwner: true}, {Identifier: "EX-3", Owner: "nix-infra", HasOwner: true},
			}},
			wantChain: []string{"EX-1", "EX-2"},
			wantOwner: "ci-build",
		},
		{
			name:      "unowned chain is reported whole and not truncated",
		in:        OwnerInput{Parents: []ParentOwner{{Identifier: "EX-1"}, {Identifier: "EX-2"}}},
		wantChain: []string{"EX-1", "EX-2"},
		},
		{
			name:          "truncated walk with no owner reports truncation",
			in:            OwnerInput{Parents: []ParentOwner{{Identifier: "EX-1"}, {Identifier: "EX-2"}}, ParentsTruncated: true},
			wantChain:     []string{"EX-1", "EX-2"},
			wantTruncated: true,
		},
		{
			name: "truncated walk that still finds an owner keeps truncation and stops the chain at the owner",
			in: OwnerInput{Parents: []ParentOwner{
				{Identifier: "EX-1", Owner: "ci-build", HasOwner: true}, {Identifier: "EX-2"},
			}, ParentsTruncated: true},
			wantChain:     []string{"EX-1"},
			wantTruncated: true,
			wantOwner:     "ci-build",
		},
		{
			name:      "no parents gives an empty chain",
			in:        OwnerInput{},
			wantChain: []string{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveOwner(tc.in)
			if strings.Join(got.Chain, ",") != strings.Join(tc.wantChain, ",") {
				t.Fatalf("Chain = %v, want %v", got.Chain, tc.wantChain)
			}
			if got.Truncated != tc.wantTruncated {
				t.Fatalf("Truncated = %v, want %v", got.Truncated, tc.wantTruncated)
			}
			if got.Owner != tc.wantOwner {
				t.Fatalf("Owner = %q, want %q", got.Owner, tc.wantOwner)
			}
		})
	}
}
