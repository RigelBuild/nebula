// SPDX-License-Identifier: MIT OR Apache-2.0

package cdgate

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Git is the command runner used to read main's history. It returns stdout.
type Git func(ctx context.Context, args ...string) (string, error)

var commitSHA = regexp.MustCompile(`^[0-9a-fA-F]{40,64}$`)

// FirstParentRanks parses `git rev-list --first-parent origin/main` output.
// The first commit is the head, rank 0, and each older commit gets the next
// rank. Empty history, a malformed SHA, or a repeated SHA is an error.
func FirstParentRanks(stdout string) (map[string]int, error) {
	commits := strings.Fields(stdout)
	if len(commits) == 0 {
		return nil, errors.New("origin/main history is empty")
	}
	ranks := make(map[string]int, len(commits))
	for index, commit := range commits {
		if !commitSHA.MatchString(commit) {
			return nil, errors.New("invalid origin/main history")
		}
		if _, dup := ranks[commit]; dup {
			return nil, errors.New("invalid origin/main history")
		}
		ranks[commit] = index
	}
	return ranks, nil
}

// MainRanks refreshes origin/main and ranks its first-parent history. A
// shallow clone is unshallowed. The fetch uses blob:none, so later history
// reads do not fetch one tree per commit.
func MainRanks(ctx context.Context, git Git) (map[string]int, error) {
	shallow, err := git(ctx, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return nil, err
	}
	switch strings.TrimSpace(shallow) {
	case "true":
		if _, err := git(ctx, "fetch", "--unshallow", "--filter=blob:none", "origin", "main"); err != nil {
			return nil, err
		}
	case "false":
		if _, err := git(ctx, "fetch", "--filter=blob:none", "origin", "main"); err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("invalid shallow repository status")
	}
	history, err := git(ctx, "rev-list", "--first-parent", "origin/main")
	if err != nil {
		return nil, fmt.Errorf("list origin/main history: %w", err)
	}
	return FirstParentRanks(history)
}
