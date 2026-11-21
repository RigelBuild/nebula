// SPDX-License-Identifier: MIT OR Apache-2.0

package issueowner

import (
	"fmt"
	"strings"
)

// ParentDepth bounds how far the sweep walks a parent chain for an owner.
const ParentDepth = 5

// OwnerSource names the fact that decided an owner.
type OwnerSource string

const (
	SourceOwner  OwnerSource = "owner"
	SourceParent OwnerSource = "parent"
	SourceBranch OwnerSource = "branch"
)

// ParentOwner is one ancestor on an issue's parent chain, as the sweep read it.
type ParentOwner struct {
	Identifier string
	Owner      string
	HasOwner   bool
}

// OwnerInput holds the pure facts ResolveOwner decides from.
// Parents are ordered nearest first and hold at most ParentDepth entries.
type OwnerInput struct {
	Owner    string
	HasOwner bool
	Parents  []ParentOwner
	// ParentsTruncated is set by the caller when its parent walk stopped at ParentDepth or on a cycle.
	ParentsTruncated bool
	// BranchCodename is the codename the sweep resolved from the issue's branch, or empty.
	BranchCodename string
}

// OwnerVerdict is the resolved owner, its evidence, any named conflict, and the parent chain read.
type OwnerVerdict struct {
	Owner    string
	Evidence string
	Conflict string
	// Source names the fact that decided the owner; empty when none did.
	Source OwnerSource
	// Chain holds the parent identifiers read, nearest first; it ends at the owning parent when one is found.
	Chain []string
	// Truncated echoes OwnerInput.ParentsTruncated; the caller's walk stopped at ParentDepth or on a cycle.
	Truncated bool
}

// ResolveOwner applies the owner precedence: own Owner line, then the nearest owned parent, then the branch codename.
// Lineage is not part of this function; the sweep applies it when ResolveOwner finds no owner.
func ResolveOwner(in OwnerInput) OwnerVerdict {
	verdict := resolveSource(in)
	verdict.Chain = parentChain(in.Parents)
	verdict.Truncated = in.ParentsTruncated
	verdict.Conflict = branchConflict(in, verdict.Owner)
	return verdict
}

func resolveSource(in OwnerInput) OwnerVerdict {
	if in.HasOwner {
		return OwnerVerdict{Owner: in.Owner, Evidence: "Owner: " + in.Owner, Source: SourceOwner}
	}
	for _, parent := range in.Parents {
		if parent.HasOwner {
			evidence := fmt.Sprintf("parent chain %s (owner %s)", strings.Join(parentChain(in.Parents), " → "), parent.Owner)
			return OwnerVerdict{Owner: parent.Owner, Evidence: evidence, Source: SourceParent}
		}
	}
	if in.BranchCodename != "" {
		return OwnerVerdict{Owner: in.BranchCodename, Evidence: "branch names " + in.BranchCodename, Source: SourceBranch}
	}
	return OwnerVerdict{}
}

// parentChain returns the identifiers read from parents, nearest first, stopping after the first owned ancestor.
func parentChain(parents []ParentOwner) []string {
	ids := make([]string, 0, len(parents))
	for _, parent := range parents {
		ids = append(ids, parent.Identifier)
		if parent.HasOwner {
			break
		}
	}
	return ids
}

// branchConflict names a branch codename that disagrees with the owner that won, or returns "".
func branchConflict(in OwnerInput, owner string) string {
	if in.BranchCodename == "" || owner == "" || in.BranchCodename == owner {
		return ""
	}
	return fmt.Sprintf("branch codename %s disagrees with owner %s", in.BranchCodename, owner)
}
